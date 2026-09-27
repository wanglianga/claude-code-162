package bus

// Kafka 主题与事件类型集中定义（无外部依赖，供 service/events 共同引用）。
const (
	TopicAppointment = "recycle.appointment"
	TopicDispatch    = "recycle.dispatch"
	TopicOnsite      = "recycle.onsite"
	TopicSettlement  = "recycle.settlement"
	TopicInbound     = "recycle.inbound"
	TopicCertificate = "recycle.certificate"
	TopicDispute     = "recycle.dispute"

	EventTypeAppointmentCreated = "AppointmentCreated"
	EventTypeAppointmentAdded   = "AppointmentItemAdded"
	EventTypeDispatchOffered    = "DispatchOffered"
	EventTypeDispatchAccepted   = "DispatchAccepted"
	EventTypeVisitVerified      = "VisitVerified"
	EventTypeWeighConfirmed     = "WeighConfirmed"
	EventTypeSettleSucceeded    = "SettlementSucceeded"
	EventTypeSettleFailed       = "SettlementFailed"
	EventTypeInboundReweighed   = "InboundReweighed"
	EventTypeCertificateIssued  = "CertificateIssued"
	EventTypeDisputeRaised      = "DisputeRaised"
	EventTypeDisputeResolved    = "DisputeResolved"
)

func AllTopics() []string {
	return []string{
		TopicAppointment, TopicDispatch, TopicOnsite, TopicSettlement,
		TopicInbound, TopicCertificate, TopicDispute,
	}
}
