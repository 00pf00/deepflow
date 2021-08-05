package datasource

import (
	"fmt"
	"strings"

	clickhouse "github.com/ClickHouse/clickhouse-go"
	"gitlab.yunshan.net/yunshan/droplet-libs/ckdb"
	"gitlab.yunshan.net/yunshan/droplet-libs/zerodoc"
	"gitlab.yunshan.net/yunshan/droplet/pkg/ckwriter"
	"gitlab.yunshan.net/yunshan/droplet/stream/common"
)

const (
	ORIGIN_TABLE_1M = "1m"
	ORIGIN_TABLE_1S = "1s"
)

// VATP_ACL数据库, 不进行数据源修改
var metricsGroupDBIDs = [][]zerodoc.MetricsDBID{
	zerodoc.VTAP_FLOW:   []zerodoc.MetricsDBID{zerodoc.VTAP_FLOW, zerodoc.VTAP_FLOW_EDGE, zerodoc.VTAP_FLOW_EDGE_PORT, zerodoc.VTAP_FLOW_PORT},
	zerodoc.VTAP_PACKET: []zerodoc.MetricsDBID{zerodoc.VTAP_PACKET, zerodoc.VTAP_PACKET_EDGE},
	zerodoc.VTAP_WAN:    []zerodoc.MetricsDBID{zerodoc.VTAP_WAN, zerodoc.VTAP_WAN_PORT},
}

func getMetricsSubDBIDs(dbGroup string) ([]zerodoc.MetricsDBID, error) {
	metricsDBID := zerodoc.MetricsDBNameToID(dbGroup)
	if metricsDBID < zerodoc.VTAP_DB_ID_MAX {
		return metricsGroupDBIDs[metricsDBID], nil
	}
	return nil, fmt.Errorf("unknown db group(%s)", dbGroup)
}

// zerodoc 的 Latency 结构中的非累加聚合字段
var unsummableMaxFieldsMap = map[string]struct{}{
	"rtt_max":        {},
	"rtt_client_max": {},
	"rtt_server_max": {},
	"srt_max":        {},
	"art_max":        {},
	"http_rrt_max":   {},
	"dns_rrt_max":    {},
}

//  对于unsumable的sum列使用max,min聚合时, count列取相应的max,min列的值
var unsummableFieldsMap = map[string]struct{}{
	"rtt_sum":        {},
	"rtt_client_sum": {},
	"rtt_server_sum": {},
	"srt_sum":        {},
	"art_sum":        {},
	"http_rrt_sum":   {},
	"dns_rrt_sum":    {},

	"rtt_count":        {},
	"rtt_client_count": {},
	"rtt_server_count": {},
	"srt_count":        {},
	"art_count":        {},
	"http_rrt_count":   {},
	"dns_rrt_count":    {},
}

func getColumnString(column *ckdb.Column, aggrSummable, aggrUnsummable string, t TableType) string {
	_, isUnsummable := unsummableFieldsMap[column.Name]
	isMaxMinAggr := (aggrUnsummable == aggrStrings[MAX]) || (aggrUnsummable == aggrStrings[MIN])
	_, isUnsummableMax := unsummableMaxFieldsMap[column.Name]

	// count字段的max,min聚合
	if isUnsummable && isMaxMinAggr {
		aggrFunc := "argMax"
		if aggrUnsummable == aggrStrings[MIN] {
			aggrFunc = "argMin"
		}
		switch t {
		case AGG:
			// 例如: rtt_count__agg AggregateFunction(argMax, UInt64, Float64),
			//   argMax的参数类型是UInt64和Float64， 当第二个参数的值最大时，rtt_count__agg 值为第一个参数的值
			return fmt.Sprintf("%s__%s AggregateFunction(%s, %s, Float64)", column.Name, AGG.String(), aggrFunc, column.Type.String()) // sum列默认是float64
		case MV:
			// 例如: argMaxState(rtt_count, rtt_sum/(rtt_count+0.01)) AS rtt_count__agg, 防止除0异常，除数加0.01
			//   表示 当 rtt_sum/(rtt_count+0.01) 为最大值时，rtt_count__agg 的值为 rtt_count的值
			return fmt.Sprintf("%sState(%s, %s/(%s+0.01)) AS %s__%s", aggrFunc, column.Name,
				strings.ReplaceAll(column.Name, "count", "sum"), strings.ReplaceAll(column.Name, "sum", "count"), // 总是取 xxx_sum/xxx_count 的值
				column.Name, AGG.String())
		case LOCAL:
			// 例如： argMaxMerge(rtt_count__agg) as rtt_count,
			return fmt.Sprintf("%sMerge(%s__%s) AS %s", aggrFunc, column.Name, AGG.String(), column.Name)
		}
	} else {
		// 普通的非累加和聚合和count字段的非max,min聚合和可累加的字段的聚合
		aggr := aggrSummable
		if isUnsummableMax || isUnsummable {
			aggr = aggrUnsummable
		}
		switch t {
		case AGG:
			return fmt.Sprintf("%s__%s AggregateFunction(%s, %s)", column.Name, t.String(), aggr, column.Type.String())
		case MV:
			return fmt.Sprintf("%sState(%s) AS %s__%s", aggr, column.Name, column.Name, AGG.String())
		case LOCAL:
			return fmt.Sprintf("%sMerge(%s__%s) AS %s", aggr, column.Name, AGG.String(), column.Name)
		}
	}

	return ""
}

type ActionEnum uint8

const (
	ADD ActionEnum = iota
	DEL
	MOD
)

var actionStrings = []string{
	ADD: "add",
	DEL: "del",
	MOD: "mod",
}

func ActionToEnum(action string) (ActionEnum, error) {
	for i, a := range actionStrings {
		if action == a {
			return ActionEnum(i), nil
		}
	}
	return 0, fmt.Errorf("unknown action %s", action)
}

type AggrEnum uint8

const (
	SUM AggrEnum = iota
	MAX
	MIN
	AVG
)

var aggrStrings = []string{
	SUM: "sum",
	MAX: "max",
	MIN: "min",
	AVG: "avg",
}

func AggrToEnum(aggr string) (AggrEnum, error) {
	for i, a := range aggrStrings {
		if aggr == a {
			return AggrEnum(i), nil
		}
	}
	return 0, fmt.Errorf("unknown aggr %s", aggr)
}

type TableType uint8

const (
	AGG    TableType = iota // 聚合后的原始表, 存储数据
	MV                      // view 无实际数据, 用来从local或agg表，读取数据写入到agg表
	LOCAL                   // view 无实际数据, 用来简化读取agg表的数据
	GLOBAL                  // 以local表为基础，建立全局表
)

var tableTypeStrings = []string{
	AGG:    "agg",
	MV:     "mv",
	LOCAL:  "local",
	GLOBAL: "",
}

func (v TableType) String() string {
	return tableTypeStrings[v]
}

type IntervalEnum uint8

const (
	IntervalHour IntervalEnum = iota
	IntervalDay
)

func getMetricsTableName(id uint8, table string, t TableType) string {
	dbId := zerodoc.MetricsDBID(id)
	if len(t.String()) == 0 {
		return fmt.Sprintf("%s.`%s`", dbId.DBName(), table)
	}
	return fmt.Sprintf("%s.`%s_%s`", dbId.DBName(), table, t.String())
}

func stringSliceHas(items []string, item string) bool {
	for _, s := range items {
		if s == item {
			return true
		}
	}
	return false
}

func makeAggTableCreateSQL(t *ckdb.Table, dstTable, aggrSummable, aggrUnsummable string, partitionTime ckdb.TimeFuncType, duration int, replicaEnabled bool) string {
	aggTable := getMetricsTableName(t.ID, dstTable, AGG)

	columns := []string{}
	orderKeys := t.OrderKeys
	for _, p := range t.Columns {
		// 跳过_开头的字段，如_tid, _id
		if strings.HasPrefix(p.Name, "_") {
			continue
		}
		codec := ""
		if p.Codec != ckdb.CodecDefault {
			codec = fmt.Sprintf("codec(%s)", p.Codec.String())
		}

		if p.GroupBy {
			if !stringSliceHas(orderKeys, p.Name) {
				orderKeys = append(orderKeys, p.Name)
			}
			columns = append(columns, fmt.Sprintf("%s %s %s", p.Name, p.Type.String(), codec))
		} else {
			columns = append(columns, getColumnString(p, aggrSummable, aggrUnsummable, AGG))
		}
	}

	engine := ckdb.AggregatingMergeTree.String()
	if replicaEnabled {
		engine = fmt.Sprintf(ckdb.ReplicatedAggregatingMergeTree.String(), t.Database, dstTable+"_"+AGG.String())
	}

	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s
				   (%s)
				   ENGINE=%s
				   PRIMARY KEY (%s)
				   ORDER BY (%s)
				   PARTITION BY %s
				   TTL %s + toIntervalDay(%d)
				   SETTINGS storage_policy = '%s'`,
		aggTable,
		strings.Join(columns, ",\n"),
		engine,
		strings.Join(t.OrderKeys[:t.PrimaryKeyCount], ","),
		strings.Join(orderKeys, ","), // 以order by的字段排序, 相同的做聚合
		partitionTime.String(t.TimeKey),
		t.TimeKey, duration,
		ckdb.DF_STORAGE_POLICY)
}

func makeMVTableCreateSQL(t *ckdb.Table, baseTable, dstTable, aggrSummable, aggrUnsummable string, aggrTimeFunc ckdb.TimeFuncType) string {
	tableMv := getMetricsTableName(t.ID, dstTable, MV)
	tableAgg := getMetricsTableName(t.ID, dstTable, AGG)

	// 对于从1m,1s表进行聚合的表，使用local表作为源表
	baseTableType := LOCAL
	columnTableType := MV
	tableBase := getMetricsTableName(t.ID, baseTable, baseTableType)

	groupKeys := t.OrderKeys
	columns := []string{}
	for _, p := range t.Columns {
		if strings.HasPrefix(p.Name, "_") {
			continue
		}
		if p.GroupBy {
			if p.Name == t.TimeKey {
				columns = append(columns, fmt.Sprintf("%s AS %s", aggrTimeFunc.String(t.TimeKey), t.TimeKey))
			} else {
				columns = append(columns, p.Name)
			}
			if !stringSliceHas(groupKeys, p.Name) {
				groupKeys = append(groupKeys, p.Name)
			}
		} else {
			columns = append(columns, getColumnString(p, aggrSummable, aggrUnsummable, columnTableType))
		}
	}

	return fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s
			AS SELECT %s
	                FROM %s
			GROUP BY (%s)
			ORDER BY (%s)`,
		tableMv, tableAgg,
		strings.Join(columns, ",\n"),
		tableBase,
		strings.Join(groupKeys, ","),
		strings.Join(t.OrderKeys, ","))
}

func makeCreateTableLocal(t *ckdb.Table, baseTable, dstTable, aggrSummable, aggrUnsummable string) string {
	tableAgg := getMetricsTableName(t.ID, dstTable, AGG)
	tableLocal := getMetricsTableName(t.ID, dstTable, LOCAL)

	columns := []string{}
	groupKeys := t.OrderKeys
	for _, p := range t.Columns {
		if strings.HasPrefix(p.Name, "_") {
			continue
		}
		if p.GroupBy {
			columns = append(columns, p.Name)
			if !stringSliceHas(groupKeys, p.Name) {
				groupKeys = append(groupKeys, p.Name)
			}
		} else {
			columns = append(columns, getColumnString(p, aggrSummable, aggrUnsummable, LOCAL))
		}
	}

	return fmt.Sprintf(`
CREATE VIEW IF NOT EXISTS %s
AS SELECT
%s
FROM %s
GROUP BY (%s)`,
		tableLocal,
		strings.Join(columns, ",\n"),
		tableAgg,
		strings.Join(groupKeys, ","))
}

func makeGlobalTableCreateSQL(t *ckdb.Table, dstTable string) string {
	tableGlobal := getMetricsTableName(t.ID, dstTable, GLOBAL)
	tableLocal := getMetricsTableName(t.ID, dstTable, LOCAL)
	engine := fmt.Sprintf(ckdb.Distributed.String(), t.Cluster, t.Database, dstTable+"_"+LOCAL.String())

	createTable := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s AS %s ENGINE = %s",
		tableGlobal, tableLocal, engine)
	return createTable
}

func getMetricsTable(id zerodoc.MetricsDBID) *ckdb.Table {
	return zerodoc.GetMetricsTables(ckdb.MergeTree)[id] // GetMetricsTables取的全局变量的值，以roze在启动时对tables初始化的参数为准
}

func createTableMV(ck clickhouse.Clickhouse, dbId zerodoc.MetricsDBID, baseTable, dstTable, aggrSummable, aggrUnsummable string, aggInterval IntervalEnum, duration int, replicaEnabled bool) error {
	table := getMetricsTable(dbId)
	if baseTable != ORIGIN_TABLE_1M && baseTable != ORIGIN_TABLE_1S {
		return fmt.Errorf("Only support base datasource 1s,1m")
	}

	aggTime := ckdb.TimeFuncHour
	partitionTime := ckdb.TimeFuncWeek
	if aggInterval == IntervalDay {
		aggTime = ckdb.TimeFuncDay
		partitionTime = ckdb.TimeFuncYYYYMM
	}

	commands := []string{
		makeAggTableCreateSQL(table, dstTable, aggrSummable, aggrUnsummable, partitionTime, duration, replicaEnabled),
		makeMVTableCreateSQL(table, baseTable, dstTable, aggrSummable, aggrUnsummable, aggTime),
		makeCreateTableLocal(table, baseTable, dstTable, aggrSummable, aggrUnsummable),
		makeGlobalTableCreateSQL(table, dstTable),
	}
	for _, cmd := range commands {
		log.Info(cmd)
		if err := ckwriter.ExecSQL(ck, cmd); err != nil {
			return err
		}
	}
	return nil
}

func modTableMV(ck clickhouse.Clickhouse, dbId zerodoc.MetricsDBID, dstTable string, duration int) error {
	table := getMetricsTable(dbId)
	tableMod := ""
	if dstTable == ORIGIN_TABLE_1M || dstTable == ORIGIN_TABLE_1S {
		tableMod = getMetricsTableName(uint8(dbId), dstTable, LOCAL)
	} else {
		tableMod = getMetricsTableName(uint8(dbId), dstTable, AGG)
	}
	modTable := fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s + toIntervalDay(%d)",
		tableMod, table.TimeKey, duration)

	return ckwriter.ExecSQL(ck, modTable)
}

func modFlowLogLocalTable(ck clickhouse.Clickhouse, tableID common.FlowLogID, duration int) error {
	timeKey := tableID.TimeKey()
	tableLocal := fmt.Sprintf("%s.%s_%s", common.FLOW_LOG_DB, tableID.String(), LOCAL)
	modTable := fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s + toIntervalDay(%d)",
		tableLocal, timeKey, duration)
	return ckwriter.ExecSQL(ck, modTable)
}

func delTableMV(ck clickhouse.Clickhouse, dbId zerodoc.MetricsDBID, table string) error {
	dropTables := []string{
		getMetricsTableName(uint8(dbId), table, GLOBAL),
		getMetricsTableName(uint8(dbId), table, LOCAL),
		getMetricsTableName(uint8(dbId), table, MV),
		getMetricsTableName(uint8(dbId), table, AGG),
	}
	for _, name := range dropTables {
		if err := ckwriter.ExecSQL(ck, "DROP TABLE IF EXISTS "+name); err != nil {
			return err
		}
	}

	return nil
}

func DatasourceHandle(ckAddrs []string, user, password, dbGroup, action, baseTable, dstTable, aggrSummable, aggrUnsummable string, interval, duration int, timeout int, replicaEnabled bool) error {
	var cks []clickhouse.Clickhouse
	for _, addr := range ckAddrs {
		if len(addr) == 0 {
			continue
		}
		ck, err := clickhouse.OpenDirect(fmt.Sprintf("%s?username=%s&password=%s&read_timeout=%d", addr, user, password, timeout))
		if err != nil {
			return err
		}
		cks = append(cks, ck)
	}
	if len(cks) == 0 {
		return fmt.Errorf("invalid clickhouse addrs: Addrs=%v ", ckAddrs)
	}

	duration = duration / 24 // 切换为天

	// flow_log只支持mod
	if dbGroup == common.FLOW_LOG_DB && action == actionStrings[MOD] {
		for _, ck := range cks {
			for flowLogID := common.L4_FLOW_ID; flowLogID < common.FLOWLOG_ID_MAX; flowLogID++ {
				if err := modFlowLogLocalTable(ck, flowLogID, duration); err != nil {
					return err
				}
			}
		}
		return nil
	}

	subDBIDs, err := getMetricsSubDBIDs(dbGroup)
	if err != nil {
		return err
	}

	actionEnum, err := ActionToEnum(action)
	if err != nil {
		return err
	}

	if actionEnum == ADD {
		if baseTable == "" {
			return fmt.Errorf("base table name is empty")
		}
		if _, err := AggrToEnum(aggrSummable); err != nil {
			return err
		}
		if _, err := AggrToEnum(aggrUnsummable); err != nil {
			return err
		}
		if interval < 1 {
			return fmt.Errorf("interval(%d) must bigger than 0.", interval)
		}
		if interval != 60 && interval != 1440 {
			return fmt.Errorf("interval(%d) only support 60 or 1440.", interval)
		}
		if duration < 1 {
			return fmt.Errorf("duration(%d) must bigger than 0.", duration)
		}
		if baseTable == dstTable {
			return fmt.Errorf("base table(%s) should not the same as the dst table(%s)", baseTable, dstTable)
		}
	}

	if dstTable == "" {
		return fmt.Errorf("dst table name is empty")
	}

	for _, ck := range cks {
		for _, dbId := range subDBIDs {
			switch actionEnum {
			case ADD:
				aggInterval := IntervalHour
				if interval == 1440 {
					aggInterval = IntervalDay
				}
				if err := createTableMV(ck, dbId, baseTable, dstTable, aggrSummable, aggrUnsummable, aggInterval, duration, replicaEnabled); err != nil {
					return err
				}
			case MOD:
				if err := modTableMV(ck, dbId, dstTable, duration); err != nil {
					return err
				}
			case DEL:
				if err := delTableMV(ck, dbId, dstTable); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupport action %s", action)
			}
		}
	}
	return nil
}
