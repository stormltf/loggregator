package message_aggregator

import (
	"github.com/cloudfoundry/dropsonde/events"
	"github.com/cloudfoundry/dropsonde/factories"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/davecgh/go-spew/spew"
	"sync"
	"time"
)

var MaxTTL = time.Minute

type MessageAggregator interface {
	instrumentation.Instrumentable
	Run(inputChan <-chan *events.Envelope, outputChan chan<- *events.Envelope)
}

func NewMessageAggregator(logger *gosteno.Logger) MessageAggregator {
	return &messageAggregator{
		logger:               logger,
		startEventsByEventId: make(map[eventId]startEventEntry),
	}
}

type messageAggregator struct {
	sync.Mutex
	logger                          *gosteno.Logger
	startEventsByEventId            map[eventId]startEventEntry
	httpStartReceivedCount          uint64
	httpStopReceivedCount           uint64
	httpStartStopEmittedCount       uint64
	uncategorizedEventCount         uint64
	httpUnmatchedStartReceivedCount uint64
	httpUnmatchedStopReceivedCount  uint64
}

type eventId struct {
	requestId string
	peerType  events.PeerType
}

type startEventEntry struct {
	startEvent *events.HttpStart
	entryTime  time.Time
}

func (m *messageAggregator) Run(inputChan <-chan *events.Envelope, outputChan chan<- *events.Envelope) {
	for envelope := range inputChan {
		// TODO: don't call for every message if throughput becomes a problem
		m.cleanupOrphanedHttpStart()

		switch envelope.GetEventType() {
		case events.Envelope_HttpStart:
			m.handleHttpStart(envelope)
		case events.Envelope_HttpStop:
			startStopMessage := m.handleHttpStop(envelope)
			if startStopMessage != nil {
				outputChan <- startStopMessage
			}
		default:
			m.incrementCounter(&m.uncategorizedEventCount)
			m.logger.Debugf("passing through message %v", spew.Sprintf("%v", envelope))
			outputChan <- envelope
		}
	}
}

func (m *messageAggregator) incrementCounter(counter *uint64) {
	m.Lock()
	defer m.Unlock()
	(*counter)++
}

func (m *messageAggregator) handleHttpStart(envelope *events.Envelope) {
	m.incrementCounter(&m.httpStartReceivedCount)

	m.logger.Debugf("handling HTTP start message %v", spew.Sprintf("%v", envelope))
	startEvent := envelope.GetHttpStart()

	requestId := factories.StringFromUUID(startEvent.RequestId)
	eventId := eventId{requestId: requestId, peerType: startEvent.GetPeerType()}
	m.startEventsByEventId[eventId] = startEventEntry{startEvent: startEvent, entryTime: time.Now()}
}

func (m *messageAggregator) handleHttpStop(envelope *events.Envelope) *events.Envelope {
	m.incrementCounter(&m.httpStopReceivedCount)

	m.logger.Debugf("handling HTTP stop message %v", spew.Sprintf("%v", envelope))
	stopEvent := envelope.GetHttpStop()

	requestId := factories.StringFromUUID(stopEvent.RequestId)
	eventId := eventId{requestId: requestId, peerType: stopEvent.GetPeerType()}

	startEventEntry, ok := m.startEventsByEventId[eventId]
	if !ok {
		m.logger.Warnf("no matching HTTP start message found for %v", eventId)
		m.incrementCounter(&m.httpUnmatchedStopReceivedCount)
		return nil
	}

	m.incrementCounter(&m.httpStartStopEmittedCount)

	delete(m.startEventsByEventId, eventId)
	startEvent := startEventEntry.startEvent

	return &events.Envelope{
		Origin:    envelope.Origin,
		EventType: events.Envelope_HttpStartStop.Enum(),
		HttpStartStop: &events.HttpStartStop{
			StartTimestamp:  startEvent.Timestamp,
			StopTimestamp:   stopEvent.Timestamp,
			RequestId:       startEvent.RequestId,
			PeerType:        startEvent.PeerType,
			Method:          startEvent.Method,
			Uri:             startEvent.Uri,
			RemoteAddress:   startEvent.RemoteAddress,
			UserAgent:       startEvent.UserAgent,
			StatusCode:      stopEvent.StatusCode,
			ContentLength:   stopEvent.ContentLength,
			ParentRequestId: startEvent.ParentRequestId,
			ApplicationId:   startEvent.ApplicationId,
			InstanceIndex:   startEvent.InstanceIndex,
			InstanceId:      startEvent.InstanceId,
		},
	}
}

func (m *messageAggregator) cleanupOrphanedHttpStart() {
	currentTime := time.Now()
	for key, eventEntry := range m.startEventsByEventId {
		if currentTime.Sub(eventEntry.entryTime) > MaxTTL {
			m.incrementCounter(&m.httpUnmatchedStartReceivedCount)
			delete(m.startEventsByEventId, key)
		}
	}
}

func (m *messageAggregator) metrics() []instrumentation.Metric {
	m.Lock()
	defer m.Unlock()

	return []instrumentation.Metric{
		instrumentation.Metric{Name: "httpStartReceived", Value: m.httpStartReceivedCount},
		instrumentation.Metric{Name: "httpStopReceived", Value: m.httpStopReceivedCount},
		instrumentation.Metric{Name: "httpStartStopEmitted", Value: m.httpStartStopEmittedCount},
		instrumentation.Metric{Name: "uncategorizedEvents", Value: m.uncategorizedEventCount},
		instrumentation.Metric{Name: "httpUnmatchedStartReceived", Value: m.httpUnmatchedStartReceivedCount},
		instrumentation.Metric{Name: "httpUnmatchedStopReceived", Value: m.httpUnmatchedStopReceivedCount},
	}
}

func (m *messageAggregator) Emit() instrumentation.Context {
	return instrumentation.Context{
		Name:    "MessageAggregator",
		Metrics: m.metrics(),
	}
}
