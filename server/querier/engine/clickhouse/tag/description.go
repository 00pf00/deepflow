package tag

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	logging "github.com/op/go-logging"

	"server/querier/config"
	"server/querier/engine/clickhouse/client"
	ckcommon "server/querier/engine/clickhouse/common"
)

var log = logging.MustGetLogger("clickhouse.tag")

// [db][table][tag]*TagDescription
type TagDescriptionKey struct {
	DB      string
	Table   string
	TagName string
}

var TAG_DESCRIPTION_KEYS = []TagDescriptionKey{}
var TAG_DESCRIPTIONS = map[TagDescriptionKey]*TagDescription{}

// key=tagEnumFile
var TAG_ENUMS = map[string][]*TagEnum{}

var tagTypeToOperators = map[string][]string{
	"resource":    []string{"=", "!=", "IN", "NOT IN", "LIKE", "NOT LIKE", "REGEXP", "NOT REGEXP"},
	"int":         []string{"=", "!=", "IN", "NOT IN", ">=", "<="},
	"int_enum":    []string{"=", "!=", "IN", "NOT IN", ">=", "<="},
	"string":      []string{"=", "!=", "IN", "NOT IN", "LIKE", "NOT LIKE", "REGEXP", "NOT REGEXP"},
	"string_enum": []string{"=", "!=", "IN", "NOT IN", "LIKE", "NOT LIKE", "REGEXP", "NOT REGEXP"},
	"ip":          []string{"=", "!=", "IN", "NOT IN", ">=", "<="},
	"mac":         []string{"=", "!=", "IN", "NOT IN"},
	"id":          []string{"=", "!=", "IN", "NOT IN"},
	"time":        []string{"=", "!=", ">=", "<="},
	"default":     []string{"=", "!="},
}
var TAG_RESOURCE_TYPE_DEVICE_MAP = map[string]int{
	"chost":       VIF_DEVICE_TYPE_VM,
	"router":      VIF_DEVICE_TYPE_VROUTER,
	"dhcpgw":      VIF_DEVICE_TYPE_DHCP_PORT,
	"pod_service": VIF_DEVICE_TYPE_POD_SERVICE,
	"redis":       VIF_DEVICE_TYPE_REDIS_INSTANCE,
	"rds":         VIF_DEVICE_TYPE_RDS_INSTANCE,
	"lb":          VIF_DEVICE_TYPE_LB,
	"natgw":       VIF_DEVICE_TYPE_NAT_GATEWAY,
	"host":        VIF_DEVICE_TYPE_HOST,
}

type TagDescription struct {
	Name        string
	ClientName  string
	ServerName  string
	DisplayName string
	Type        string
	EnumFile    string
	Category    string
	Description string
	Operators   []string
	Permissions []bool
}

func NewTagDescription(
	name, clientName, serverName, displayName, tagType, enumFile, category string,
	permissions []bool, description string,
) *TagDescription {
	operators, ok := tagTypeToOperators[tagType]
	if !ok {
		operators, _ = tagTypeToOperators["default"]
	}
	return &TagDescription{
		Name:        name,
		ClientName:  clientName,
		ServerName:  serverName,
		DisplayName: displayName,
		Type:        tagType,
		EnumFile:    enumFile,
		Category:    category,
		Operators:   operators,
		Permissions: permissions,
		Description: description,
	}
}

type TagEnum struct {
	Value       interface{}
	DisplayName interface{}
}

func NewTagEnum(value, displayName interface{}) *TagEnum {
	return &TagEnum{
		Value:       value,
		DisplayName: displayName,
	}
}

func LoadTagDescriptions(tagData map[string]interface{}) error {
	// 生成tag description
	enumFileToTagType := make(map[string]string)
	for db, dbTagData := range tagData {
		if db == "enum" {
			continue
		}
		for table, tableTagData := range dbTagData.(map[string]interface{}) {
			// 遍历文件内容进行赋值
			for _, tag := range tableTagData.([][]interface{}) {
				if len(tag) < 9 {
					return errors.New(
						fmt.Sprintf("get tag failed! db:%s table:%s, tag:%v", db, table, tag),
					)
				}
				// 0 - Name
				// 1 - ClientName
				// 2 - ServerName
				// 3 - DisplayName
				// 4 - Type
				// 5 - EnumFile
				// 6 - Category
				// 7 - Permissions
				// 8 - Description
				permissions, err := ckcommon.ParsePermission(tag[7])
				if err != nil {
					return errors.New(
						fmt.Sprintf(
							"parse tag permission failed! db:%s table:%s, tag:%v, err:%s",
							db, table, tag, err.Error(),
						),
					)
				}

				key := TagDescriptionKey{DB: db, Table: table, TagName: tag[0].(string)}
				TAG_DESCRIPTION_KEYS = append(TAG_DESCRIPTION_KEYS, key)
				description := NewTagDescription(
					tag[0].(string), tag[1].(string), tag[2].(string), tag[3].(string),
					tag[4].(string), tag[5].(string), tag[6].(string), permissions, tag[8].(string),
				)
				TAG_DESCRIPTIONS[key] = description
				enumFileToTagType[tag[5].(string)] = tag[4].(string)
			}
		}
	}

	// 生成tag enum值
	tagEnumData, ok := tagData["enum"]
	if ok {
		for tagEnumFile, enumData := range tagEnumData.(map[string]interface{}) {
			tagEnums := []*TagEnum{}
			// 根据tagEnumFile获取tagTypeToOperators
			tagType, _ := enumFileToTagType[tagEnumFile]

			for _, enumValue := range enumData.([][]interface{}) {
				// 如果是int/int_enum，则将value转为interface
				if tagType == "int" || tagType == "int_enum" {
					value, _ := strconv.Atoi(enumValue[0].(string))
					tagEnums = append(tagEnums, NewTagEnum(value, enumValue[1]))
				} else {
					tagEnums = append(tagEnums, NewTagEnum(enumValue[0], enumValue[1]))
				}
			}
			TAG_ENUMS[tagEnumFile] = tagEnums
		}
	} else {
		return errors.New("get tag enum failed! ")
	}
	return nil
}

func GetTagDescriptions(db, table string) (map[string][]interface{}, error) {
	// 把`1m`的反引号去掉
	table = strings.Trim(table, "`")
	response := map[string][]interface{}{
		"columns": []interface{}{
			"name", "client_name", "server_name", "display_name", "type", "category",
			"operators", "permissions", "description",
		},
		"values": []interface{}{},
	}

	for _, key := range TAG_DESCRIPTION_KEYS {
		if key.DB != db || key.Table != table {
			continue
		}
		tag, _ := TAG_DESCRIPTIONS[key]
		response["values"] = append(
			response["values"],
			[]interface{}{
				tag.Name, tag.ClientName, tag.ServerName, tag.DisplayName, tag.Type,
				tag.Category, tag.Operators, tag.Permissions, tag.Description,
			},
		)
	}

	// 查询 k8s_label
	chClient := client.Client{
		Host:     config.Cfg.Clickhouse.Host,
		Port:     config.Cfg.Clickhouse.Port,
		UserName: config.Cfg.Clickhouse.User,
		Password: config.Cfg.Clickhouse.Password,
		DB:       "deepflow",
	}
	err := chClient.Init("")
	if err != nil {
		return nil, err
	}
	sql := "SELECT key FROM k8s_label_map GROUP BY key"
	rst, err := chClient.DoQuery(sql, nil)
	if err != nil {
		return nil, err
	}
	for _, _key := range rst["values"] {
		key := _key.([]interface{})[0]
		labelKey := "label." + key.(string)
		response["values"] = append(response["values"], []interface{}{
			labelKey, labelKey + "_0", labelKey + "_1", labelKey, "label",
			"标签", tagTypeToOperators["string"], []bool{true, true, true}, "",
		})
	}

	// 查询外部字段
	if (db != "ext_metrics" && db != "flow_log") || (db == "flow_log" && table != "l7_flow_log") {
		return response, nil
	}
	externalChClient := client.Client{
		Host:     config.Cfg.Clickhouse.Host,
		Port:     config.Cfg.Clickhouse.Port,
		UserName: config.Cfg.Clickhouse.User,
		Password: config.Cfg.Clickhouse.Password,
		DB:       db,
	}
	err = externalChClient.Init("")
	if err != nil {
		return nil, err
	}
	externalSql := fmt.Sprintf("SELECT arrayJoin(tag_names) AS tag_name FROM (SELECT tag_names FROM %s LIMIT 1) GROUP BY tag_name", table)
	externalRst, err := externalChClient.DoQuery(externalSql, nil)
	if err != nil {
		return nil, err
	}
	for _, _tagName := range externalRst["values"] {
		tagName := _tagName.([]interface{})[0]
		if db == "ext_metrics" {
			externalTag := "tag." + tagName.(string)
			response["values"] = append(response["values"], []interface{}{
				externalTag, externalTag, externalTag, externalTag, "tag",
				"原始Tag", tagTypeToOperators["string"], []bool{true, true, true}, externalTag,
			})
		} else {
			externalTag := "attribute." + tagName.(string)
			response["values"] = append(response["values"], []interface{}{
				externalTag, externalTag, externalTag, externalTag, "attribute",
				"原始Attribute", tagTypeToOperators["string"], []bool{true, true, true}, externalTag,
			})
		}
	}
	return response, nil
}

func GetTagValues(db, table, sql string) (map[string][]interface{}, error) {
	// 把`1m`的反引号去掉
	table = strings.Trim(table, "`")
	// 获取tagEnumFile
	sqlSplit := strings.Split(sql, " ")
	tag := sqlSplit[2]
	tag = strings.Trim(tag, "'")
	// 标签是动态的,不需要去tag_description里确认
	if strings.HasPrefix(tag, "label.") {
		return GetTagResourceValues(sql)
	}
	// 外部字段是动态的,不需要去tag_description里确认
	if strings.HasPrefix(tag, "tag.") || strings.HasPrefix(tag, "attribute.") {
		return GetExternalTagValues(db, table, sql)
	}
	if db == "ext_metrics" {
		table = "ext_common"
	}
	tagDescription, ok := TAG_DESCRIPTIONS[TagDescriptionKey{
		DB: db, Table: table, TagName: tag,
	}]
	if !ok {
		return nil, errors.New(fmt.Sprintf("no tag %s in %s.%s", tag, db, table))
	}
	// 根据tagEnumFile获取values
	tagValues, ok := TAG_ENUMS[tagDescription.EnumFile]
	if !ok {
		return GetTagResourceValues(sql)
	}
	response := map[string][]interface{}{
		"columns": []interface{}{"value", "display_name"},
		"values":  []interface{}{},
	}
	for _, value := range tagValues {
		response["values"] = append(
			response["values"], []interface{}{value.Value, value.DisplayName},
		)
	}
	return response, nil
}

func GetTagResourceValues(rawSql string) (map[string][]interface{}, error) {
	chClient := client.Client{
		Host:     config.Cfg.Clickhouse.Host,
		Port:     config.Cfg.Clickhouse.Port,
		UserName: config.Cfg.Clickhouse.User,
		Password: config.Cfg.Clickhouse.Password,
		DB:       "deepflow",
	}
	sqlSplit := strings.Split(rawSql, " ")
	tag := sqlSplit[2]
	tag = strings.Trim(tag, "'")
	var whereSql string
	if strings.Contains(rawSql, "WHERE") {
		whereSql = strings.Split(rawSql, "WHERE")[1]
	} else {
		if tag == "ip" || strings.HasPrefix(tag, "label.") {
			whereSql = "value!=''"
		} else {
			whereSql = "value!=0"
		}
	}
	err := chClient.Init("")
	if err != nil {
		return nil, err
	}
	var sql string
	var dictTag = "''"
	switch tag {
	case "resource_gl0", "resource_gl1", "resource_gl2":
		results := map[string][]interface{}{}
		for resourceKey, resourceType := range AutoMap {
			// 增加资源ID
			switch resourceKey {
			case "chost", "rds", "redis", "lb", "natgw":
				dictTag = fmt.Sprintf("dictGet(deepflow.device_map, ('uid'), (toUInt64(%s), toUInt64(value)))", strconv.Itoa(resourceType))
			case "vpc":
				dictTag = fmt.Sprintf("dictGet(deepflow.l3_epc_map, ('uid'), toUInt64(value)))")
			}
			resourceId := resourceKey + "_id"
			resourceName := resourceKey + "_name"
			sql = fmt.Sprintf("SELECT %s AS value,%s AS display_name, %s AS device_type, %s AS uid FROM ip_resource_map WHERE %s GROUP BY value, display_name ORDER BY value ASC", resourceId, resourceName, strconv.Itoa(resourceType), dictTag, whereSql)
			log.Debug(sql)
			rst, err := chClient.DoQuery(sql, nil)
			if err != nil {
				return nil, err
			}

			for _, value := range rst["values"] {
				results["values"] = append(results["values"], value)
			}
		}
		autoMap := map[string]map[string]int{
			"resource_gl0": AutoPodMap,
			"resource_gl1": AutoPodGroupMap,
			"resource_gl2": AutoServiceMap,
		}
		for resourceKey, resourceType := range autoMap[tag] {
			resourceId := resourceKey + "_id"
			resourceName := resourceKey + "_name"
			if resourceKey == "service" {
				resourceId = "pod_service_id"
				resourceName = "pod_service_name"
			}
			sql = fmt.Sprintf("SELECT %s AS value,%s AS display_name, %s AS device_type, %s AS uid FROM ip_resource_map WHERE %s GROUP BY value, display_name ORDER BY value ASC", resourceId, resourceName, strconv.Itoa(resourceType), dictTag, whereSql)
			log.Debug(sql)
			rst, err := chClient.DoQuery(sql, nil)
			if err != nil {
				return nil, err
			}
			for _, value := range rst["values"] {
				results["values"] = append(results["values"], value)
			}
			results["columns"] = rst["columns"]
		}
		return results, nil
	}

	switch tag {
	case "chost", "rds", "redis", "lb", "natgw":
		resourceId := tag + "_id"
		resourceName := tag + "_name"
		dictTag = fmt.Sprintf("dictGet(deepflow.device_map, ('uid'), (toUInt64(%s), toUInt64(value)))", strconv.Itoa(AutoMap[tag]))
		sql = fmt.Sprintf("SELECT %s AS value,%s AS display_name, %s AS uid FROM ip_resource_map WHERE %s GROUP BY value, display_name ORDER BY value ASC", resourceId, resourceName, dictTag, whereSql)

	case "vpc":
		sql = fmt.Sprintf("SELECT vpc_id AS value, vpc_name AS display_name, dictGet(deepflow.l3_epc_map, 'uid', toUInt64(value)) AS uid FROM ip_resource_map WHERE %s GROUP BY value, display_name ORDER BY value ASC", whereSql)

	case "router", "host", "dhcpgw", "pod_service", "ip", "l2_vpc", "lb_listener", "pod_ingress", "az", "region", "pod_cluster", "pod_ns", "pod_node", "pod_group", "pod", "subnet":
		resourceId := tag + "_id"
		resourceName := tag + "_name"
		if tag == "l2_vpc" {
			resourceId = "vpc_id"
			resourceName = "vpc_name"
		} else if tag == "ip" {
			resourceId = "ip"
			resourceName = "ip"
		}
		sql = fmt.Sprintf("SELECT %s AS value,%s AS display_name FROM ip_resource_map WHERE %s GROUP BY value, display_name ORDER BY value ASC", resourceId, resourceName, whereSql)

	case "tap":
		sql = "SELECT value, name AS display_name FROM tap_type_map GROUP BY value, display_name ORDER BY value ASC"

	case "vtap":
		sql = "SELECT id AS value, name AS display_name FROM vtap_map GROUP BY value, display_name ORDER BY value ASC"

	default:
		if strings.HasPrefix(tag, "label.") {
			labelTag := strings.TrimPrefix(tag, "label.")
			sql = fmt.Sprintf("SELECT value, value AS display_name FROM k8s_label_map WHERE key='%s' AND %s GROUP BY value, display_name ORDER BY value ASC", labelTag, whereSql)
			log.Info(sql)
		} else {
			return map[string][]interface{}{}, nil
		}

	}
	log.Debug(sql)
	rst, err := chClient.DoQuery(sql, nil)
	if err != nil {
		return nil, err
	}
	return rst, err
}

func GetExternalTagValues(db, table, rawSql string) (map[string][]interface{}, error) {
	chClient := client.Client{
		Host:     config.Cfg.Clickhouse.Host,
		Port:     config.Cfg.Clickhouse.Port,
		UserName: config.Cfg.Clickhouse.User,
		Password: config.Cfg.Clickhouse.Password,
		DB:       db,
	}
	sqlSplit := strings.Split(rawSql, " ")
	tag := sqlSplit[2]
	tag = strings.Trim(tag, "'")
	tag = strings.TrimPrefix(tag, "tag.")
	tag = strings.TrimPrefix(tag, "attribute.")
	var whereSql string
	if strings.Contains(rawSql, "WHERE") {
		whereSql = strings.Split(rawSql, "WHERE")[1]
		whereSql = whereSql + fmt.Sprintf("AND tag_name='%s'", tag)
	} else {
		whereSql = fmt.Sprintf("tag_name='%s'", tag)
	}
	err := chClient.Init("")
	if err != nil {
		return nil, err
	}
	var sql string
	sql = fmt.Sprintf("WITH arrayJoin(tag_names) AS tag_name SELECT arrayJoin(tag_values) AS value, value AS display_name FROM %s WHERE %s GROUP BY value, display_name ORDER BY value ASC", table, whereSql)
	log.Debug(sql)
	rst, err := chClient.DoQuery(sql, nil)
	if err != nil {
		return nil, err
	}
	return rst, err
}
