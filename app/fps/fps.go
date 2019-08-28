package fps

import (
	"gitlab.x.lan/yunshan/droplet-libs/app"
	inputtype "gitlab.x.lan/yunshan/droplet-libs/datatype"
	"gitlab.x.lan/yunshan/droplet-libs/utils"
	outputtype "gitlab.x.lan/yunshan/droplet-libs/zerodoc"

	. "gitlab.x.lan/yunshan/droplet/app/common/docbuffer"
	. "gitlab.x.lan/yunshan/droplet/app/common/doctime"
	. "gitlab.x.lan/yunshan/droplet/app/common/endpoint"
	. "gitlab.x.lan/yunshan/droplet/app/common/flow"
	. "gitlab.x.lan/yunshan/droplet/app/common/policy"

	"github.com/google/gopacket/layers"
	"github.com/op/go-logging"
)

var log = logging.MustGetLogger("fps")

const (
	MINUTE      = 60
	KEY_SECONDS = 4 // 开始、结束、两个分钟的0秒
	CODES_LEN   = 64
)

var (
	// 整个生命周期为1秒的流
	ONE_SECOND_METER = outputtype.FPSMeter{
		SumFlowCount:       1,
		SumNewFlowCount:    1,
		SumClosedFlowCount: 1,
	}
	// 整个生命周期大于1秒的流，于第一个周期内开始时刻的统计量
	FIRST_METER = outputtype.FPSMeter{
		SumFlowCount:    1,
		SumNewFlowCount: 1,
	}
	// 整个生命周期大于1秒的流，排除第一个周期内开始时刻和最后一个周期内结束时刻，于每分钟第0秒的统计量
	MIDDLE_METER = outputtype.FPSMeter{
		SumFlowCount: 1,
	}
	// 整个生命周期大于1秒、本次上报周期大于1秒、且已结束的流
	LAST_METER = outputtype.FPSMeter{
		SumFlowCount:       1,
		SumClosedFlowCount: 1,
	}
	// 整个生命周期大于1秒、本次上报周期为1秒、且已结束的流
	RESIDUAL_LAST_METER = outputtype.FPSMeter{
		SumClosedFlowCount: 1,
	}
)

// node

var NODE_CODES = []outputtype.Code{}

var STAT_CODES_LEN = len(NODE_CODES)

// policy node

var POLICY_NODE_CODES = []outputtype.Code{
	outputtype.IndexToCode(0x00) | outputtype.ACLDirection | outputtype.ACLGID | outputtype.Direction | outputtype.IP | outputtype.TAPType,
}

var POLICY_NODE_CODES_LEN = len(POLICY_NODE_CODES)

// policy edge

var POLICY_EDGE_CODES = []outputtype.Code{
	outputtype.IndexToCode(0x01) | outputtype.ACLDirection | outputtype.ACLGID | outputtype.Direction | outputtype.IPPath | outputtype.TAPType,
}

var POLICY_EDGE_CODES_LEN = len(POLICY_EDGE_CODES)

type FlowToFPSDocumentMapper struct {
	timestamps  []uint32
	fields      [2]outputtype.Field
	meters      []*outputtype.FPSMeter
	docs        *utils.StructBuffer
	policyGroup []inputtype.AclAction
	codes       []outputtype.Code
}

func (p *FlowToFPSDocumentMapper) GetName() string {
	return "FlowToFPSDocumentMapper"
}

func NewProcessor() app.FlowProcessor {
	return &FlowToFPSDocumentMapper{}
}

func (p *FlowToFPSDocumentMapper) Prepare() {
	p.timestamps = make([]uint32, 0, KEY_SECONDS)
	p.meters = make([]*outputtype.FPSMeter, 0, KEY_SECONDS)

	p.docs = NewMeterSharedDocBuffer()
	p.policyGroup = make([]inputtype.AclAction, 0)
	p.codes = make([]outputtype.Code, 0, CODES_LEN)
}

func (p *FlowToFPSDocumentMapper) appendDocs(field *outputtype.Field, code outputtype.Code, actionFlags uint32) {
	for k := range p.timestamps {
		doc := p.docs.Get().(*app.Document)
		field.FillTag(code, doc.Tag.(*outputtype.Tag))
		doc.Meter = p.meters[k]
		doc.Timestamp = p.timestamps[k]
		doc.ActionFlags = actionFlags
	}
}

func (p *FlowToFPSDocumentMapper) Process(rawFlow *inputtype.TaggedFlow, variedTag bool) []interface{} {
	p.docs.Reset()

	if rawFlow.EthType != layers.EthernetTypeIPv4 {
		return p.docs.Slice()
	}

	actionFlags := rawFlow.PolicyData.ActionFlags
	interestActions := inputtype.ACTION_FLOW_COUNTING
	if actionFlags&interestActions == 0 {
		return p.docs.Slice()
	}

	statTemplates := GetTagTemplateByActionFlags(rawFlow.PolicyData, interestActions)
	p.policyGroup = FillPolicyTagTemplate(rawFlow.PolicyData, interestActions, p.policyGroup)

	oneSideCodes := make([]outputtype.Code, 0, STAT_CODES_LEN)
	if statTemplates&inputtype.TEMPLATE_NODE != 0 {
		oneSideCodes = append(oneSideCodes, NODE_CODES...)
	}

	flow := Flow(*rawFlow)
	l3EpcIDs := [2]int32{flow.FlowMetricsPeerSrc.L3EpcID, flow.FlowMetricsPeerDst.L3EpcID}
	isNorthSouthTraffic := IsNorthSourceTraffic(l3EpcIDs[0], l3EpcIDs[1])
	ips := [2]uint32{flow.IPSrc, flow.IPDst}
	isL2L3End := [2]bool{
		flow.FlowMetricsPeerSrc.IsL2End && flow.FlowMetricsPeerSrc.IsL3End,
		flow.FlowMetricsPeerDst.IsL2End && flow.FlowMetricsPeerDst.IsL3End,
	}
	directions := [2]outputtype.DirectionEnum{outputtype.ClientToServer, outputtype.ServerToClient}
	startTimestamp := RoundToSecond(flow.StartTime)
	endTimestamp := RoundToSecond(flow.EndTime)

	for i := range ips {
		if IsOuterPublicIp(l3EpcIDs[i]) {
			ips[i] = 0
		}
	}

	newFlowCount := flow.NewFlowCount()
	closedFlowCount := flow.ClosedFlowCount()

	timestamps := p.timestamps[:0]
	meters := p.meters[:0]
	// 仅需统计流整个生命周期的第一秒、最后一秒，以及每分钟的第0秒。
	// 若Force Report流丢失时意味着平台压力大，这样的精简也有助于平台降压恢复。
	if startTimestamp == endTimestamp {
		if newFlowCount == 1 && closedFlowCount == 1 {
			// 整个生命周期为1秒
			timestamps = append(timestamps, endTimestamp)
			meters = append(meters, &ONE_SECOND_METER)
		} else if newFlowCount == 1 {
			// 整个生命周期大于1秒，第一个周期开始时刻
			timestamps = append(timestamps, endTimestamp)
			meters = append(meters, &FIRST_METER)
		} else if closedFlowCount == 1 {
			// 整个生命周期大于1秒，最后一个周期结束时刻
			// 只需补齐endTimestamp所在秒的closedFlowCount残差即可，避免统计量过大
			timestamps = append(timestamps, endTimestamp)
			meters = append(meters, &RESIDUAL_LAST_METER)
		} else {
			// 上一个周期已经统计过
		}
	} else {
		if newFlowCount == 1 {
			// 整个生命周期大于1秒，第一个周期开始时刻
			timestamps = append(timestamps, startTimestamp)
			meters = append(meters, &FIRST_METER)
		}
		if startTimestamp/MINUTE != endTimestamp/MINUTE {
			// 跨越了一个自然分钟时，统计其间每分钟的第0秒。其中对于最后一秒：
			//   若为整个生命周期的最后一秒，不在此处统计（1 - closedFlowCount = 0）
			//   若不是整个生命周期的最后一秒，在此处统计（1 - closedFlowCount = 1）
			end := endTimestamp + 1 - uint32(closedFlowCount)
			for ts := startTimestamp/MINUTE*MINUTE + MINUTE; ts < end; ts += MINUTE {
				timestamps = append(timestamps, ts)
				meters = append(meters, &MIDDLE_METER)
			}
		}
		if closedFlowCount == 1 {
			// 整个生命周期大于1秒，最后一个周期结束时刻
			timestamps = append(timestamps, endTimestamp)
			meters = append(meters, &LAST_METER)
		}
	}
	p.timestamps = timestamps
	p.meters = meters

	for _, thisEnd := range [...]EndPoint{ZERO, ONE} {
		otherEnd := GetOppositeEndpoint(thisEnd)

		field := &p.fields[thisEnd]
		field.IP = ips[thisEnd]
		field.IP1 = ips[otherEnd]
		field.TAPType = TAPTypeFromInPort(flow.InPort)
		field.ACLDirection = outputtype.ACL_FORWARD // 含ACLDirection字段时仅考虑ACL正向匹配
		field.Direction = directions[thisEnd]

		// policy
		for _, policy := range p.policyGroup {
			field.ACLGID = uint16(policy.GetACLGID())

			// node
			codes := p.codes[:0]
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_NODE != 0 {
				codes = append(codes, POLICY_NODE_CODES...)
			}
			for _, code := range codes {
				if IsDupTraffic(flow.InPort, isL2L3End[thisEnd], isL2L3End[otherEnd], isNorthSouthTraffic, code) {
					continue
				}
				if IsWrongEndPointWithACL(thisEnd, policy.GetDirections(), code) {
					continue
				}
				p.appendDocs(field, code, uint32(policy.GetActionFlags()))
			}

			// edge
			codes = p.codes[:0]
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_EDGE != 0 {
				codes = append(codes, POLICY_EDGE_CODES...)
			}
			for _, code := range codes {
				if IsDupTraffic(flow.InPort, isL2L3End[thisEnd], isL2L3End[otherEnd], isNorthSouthTraffic, code) {
					continue
				}
				if IsWrongEndPointWithACL(thisEnd, policy.GetDirections(), code) {
					continue
				}
				p.appendDocs(field, code, uint32(policy.GetActionFlags()))
			}
		}
	}
	return p.docs.Slice()
}
