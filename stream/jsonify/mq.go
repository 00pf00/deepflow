package jsonify

import (
	"fmt"
	"time"

	"gitlab.yunshan.net/yunshan/droplet-libs/ckdb"
	"gitlab.yunshan.net/yunshan/droplet-libs/datatype"
	"gitlab.yunshan.net/yunshan/droplet-libs/grpc"
	"gitlab.yunshan.net/yunshan/droplet-libs/pool"
)

const (
	KAFKA_API_FETCH = 1 // https://kafka.apache.org/protocol#protocol_api_keys
)

// MQ
type MQLogger struct {
	pool.ReferenceCount
	_id uint64

	L7Base

	*datatype.AppProtoLogsData
	kafka *datatype.KafkaInfo
}

func MQLoggerColumns() []*ckdb.Column {
	columns := []*ckdb.Column{}
	columns = append(columns, ckdb.NewColumn("_id", ckdb.UInt64).SetCodec(ckdb.CodecDoubleDelta))
	columns = append(columns, L7BaseColumns()...)
	columns = append(columns,
		ckdb.NewColumn("l7_protocol", ckdb.UInt8).SetComment("应用协议, 5: Dubbo"),
		ckdb.NewColumn("type", ckdb.UInt8).SetComment("报文类型, 0: 请求, 1: 回复, 2: 会话"),
		ckdb.NewColumn("request_id", ckdb.UInt32).SetComment("请求ID, kafka: correlation_id"),

		ckdb.NewColumn("command", ckdb.LowCardinalityString).SetComment("命令类型"),
		ckdb.NewColumn("status_code", ckdb.UInt8).SetComment("状态, 0: 正常 1: 异常"),
		ckdb.NewColumn("answer_code", ckdb.Int16Nullable).SetComment("响应码"),
		ckdb.NewColumn("exception_desc", ckdb.LowCardinalityString).SetComment("异常描述"),

		ckdb.NewColumn("duration", ckdb.UInt64).SetComment("响应时延(us)"),
		ckdb.NewColumn("request_length", ckdb.UInt32).SetComment("请求长度"),
		ckdb.NewColumn("response_length", ckdb.UInt32).SetComment("响应长度"),
	)
	return columns
}

func (s *MQLogger) WriteBlock(block *ckdb.Block) error {
	if err := block.WriteUInt64(s._id); err != nil {
		return err
	}

	if err := s.L7Base.WriteBlock(block); err != nil {
		return nil
	}
	if s.kafka != nil {
		if err := block.WriteUInt8(uint8(datatype.L7_PROTOCOL_KAFKA)); err != nil {
			return err
		}
		msgType := s.AppProtoLogsData.AppProtoLogsBaseInfo.MsgType
		if err := block.WriteUInt8(uint8(msgType)); err != nil {
			return err
		}
		if err := block.WriteUInt32(s.kafka.CorrelationId); err != nil {
			return err
		}
		// 请求时有
		apiKey := ""
		if msgType == datatype.MSG_T_REQUEST || msgType == datatype.MSG_T_SESSION {
			apiKey = KafkaCommand(s.kafka.ApiKey).String()
		}
		if err := block.WriteString(apiKey); err != nil {
			return err
		}

		status := s.AppProtoLogsData.AppProtoLogsBaseInfo.Status
		if msgType == datatype.MSG_T_REQUEST {
			status = datatype.STATUS_NOT_EXIST
		}
		if err := block.WriteUInt8(status); err != nil {
			return err
		}

		answerCode := int16(s.AppProtoLogsData.AppProtoLogsBaseInfo.Code)
		answerCodePtr := &answerCode
		if msgType == datatype.MSG_T_REQUEST || answerCode == int16(NONE) {
			answerCodePtr = nil
		}
		if err := block.WriteInt16Nullable(answerCodePtr); err != nil {
			return err
		}

		execptionDesc := ""
		if answerCodePtr != nil {
			if answerCode <= TRANSACTIONAL_ID_NOT_FOUND && answerCode >= 0 {
				execptionDesc = kafkaExceptionDesc[answerCode]
			} else if answerCode == UNKNOWN_SERVER_ERROR {
				execptionDesc = UNKNOWN_SERVER_ERROR_DESC
			}
		}
		if err := block.WriteString(execptionDesc); err != nil {
			return err
		}

		if err := block.WriteUInt64(uint64(s.AppProtoLogsData.AppProtoLogsBaseInfo.RRT / time.Microsecond)); err != nil {
			return err
		}
		var requestLen, responseLen uint32
		if msgType == datatype.MSG_T_REQUEST || msgType == datatype.MSG_T_SESSION {
			requestLen = s.kafka.ReqMsgSize
		}

		if msgType == datatype.MSG_T_RESPONSE || msgType == datatype.MSG_T_SESSION {
			responseLen = s.kafka.RespMsgSize
		}
		if err := block.WriteUInt32(requestLen); err != nil {
			return err
		}
		if err := block.WriteUInt32(responseLen); err != nil {
			return err
		}
	}

	return nil
}

func (s *MQLogger) Fill(l *datatype.AppProtoLogsData, platformData *grpc.PlatformInfoTable) {
	s.L7Base.Fill(l, platformData)

	if l.Proto == datatype.PROTO_KAFKA {
		if info, ok := l.Detail.(*datatype.KafkaInfo); ok {
			s.kafka = info
		}
	}
}

func (s *MQLogger) Release() {
	ReleaseMQLogger(s)
}

func (s *MQLogger) EndTime() time.Duration {
	return time.Duration(s.L7Base.EndTime) * time.Microsecond
}

func (s *MQLogger) String() string {
	return fmt.Sprintf("MQ: %+v\n", *s)
}

var poolMQLogger = pool.NewLockFreePool(func() interface{} {
	return new(MQLogger)
})

func AcquireMQLogger() *MQLogger {
	l := poolMQLogger.Get().(*MQLogger)
	l.ReferenceCount.Reset()
	return l
}

func ReleaseMQLogger(l *MQLogger) {
	if l == nil {
		return
	}
	if l.SubReferenceCount() {
		return
	}
	if l.AppProtoLogsData != nil {
		l.AppProtoLogsData.Release()
	}
	*l = MQLogger{}
	poolMQLogger.Put(l)
}

var L7MQCounter uint32

func ProtoLogToMQLogger(l *datatype.AppProtoLogsData, shardID int, platformData *grpc.PlatformInfoTable) interface{} {
	h := AcquireMQLogger()
	l.AddReferenceCount()
	h.AppProtoLogsData = l
	h._id = genID(uint32(l.StartTime/time.Second), &L7MQCounter, shardID)
	h.Fill(l, platformData)
	return h
}
