package sinkmanager_test

import (
	"github.com/cloudfoundry/loggregatorlib/appservice"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/cloudfoundry/loggregatorlib/loggertesthelper"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"loggregator/iprange"
	"loggregator/sinks"
	"loggregator/sinks/dump"
	"loggregator/sinks/syslog"
	"loggregator/sinks/syslogwriter"
	"loggregator/sinkserver/blacklist"
	"loggregator/sinkserver/metrics"
	"loggregator/sinkserver/sinkmanager"
	"net/url"
	"sync"
	"time"
)

type ChannelSink struct {
	sync.RWMutex
	done              chan struct{}
	appId, identifier string
	received          []*logmessage.Message
}

func (c *ChannelSink) AppId() string { return c.appId }
func (c *ChannelSink) Run(msgChan <-chan *logmessage.Message) {
	defer close(c.done)
	for msg := range msgChan {
		c.Lock()
		c.received = append(c.received, msg)
		c.Unlock()
	}
}

func (c *ChannelSink) RunFinished() bool {
	<-c.done
	return true
}

func (c *ChannelSink) Received() []*logmessage.Message {
	c.RLock()
	defer c.RUnlock()
	data := make([]*logmessage.Message, len(c.received))
	copy(data, c.received)
	return data
}

func (c *ChannelSink) Identifier() string        { return c.identifier }
func (c *ChannelSink) ShouldReceiveErrors() bool { return true }
func (c *ChannelSink) Emit() instrumentation.Context {
	return instrumentation.Context{}
}

var _ = Describe("SinkManager", func() {
	var blackListManager = blacklist.New([]iprange.IPRange{iprange.IPRange{Start: "10.10.10.10", End: "10.10.10.20"}})
	var sinkManager *sinkmanager.SinkManager
	var appServicesChan <-chan appservice.AppServices
	var sinkManagerDone chan struct{}
	var newAppServiceChan, deletedAppServiceChan chan appservice.AppService

	BeforeEach(func() {
		sinkManager, appServicesChan = sinkmanager.NewSinkManager(1, true, blackListManager, loggertesthelper.Logger())

		newAppServiceChan = make(chan appservice.AppService)
		deletedAppServiceChan = make(chan appservice.AppService)

		sinkManagerDone = make(chan struct{})
		go func() {
			defer close(sinkManagerDone)
			sinkManager.Start(newAppServiceChan, deletedAppServiceChan)
		}()
	})

	AfterEach(func() {
		sinkManager.Stop()
		<-sinkManagerDone
	})

	Describe("SendSyslogErrorToLoggregator", func() {
		It("should listen and broadcast error messages", func() {

			sink := &ChannelSink{appId: "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
			sinkManager.SendSyslogErrorToLoggregator("error msg", "myApp")

			Eventually(sink.Received).Should(HaveLen(1))

			errorMsg := sink.Received()[0]
			Expect(string(errorMsg.GetLogMessage().GetMessage())).To(Equal("error msg"))

		})
	})

	Describe("SendTo", func() {
		It("should send to all known sinks", func() {

			sink1 := &ChannelSink{appId: "myApp",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sink2 := &ChannelSink{appId: "myApp",
				identifier: "myAppChan2",
				done:       make(chan struct{}),
			}

			sinkManager.RegisterSink(sink1)
			sinkManager.RegisterSink(sink2)

			expectedMessageString := "Some Data"
			expectedMessage := NewMessage(expectedMessageString, "myApp")
			go sinkManager.SendTo("myApp", expectedMessage)

			Eventually(sink1.Received).Should(HaveLen(1))
			Eventually(sink2.Received).Should(HaveLen(1))
			Expect(sink1.Received()[0]).To(Equal(expectedMessage))
			Expect(sink2.Received()[0]).To(Equal(expectedMessage))

		})

		It("should only send to sinks that match the appID", func(done Done) {

			sink1 := &ChannelSink{appId: "myApp1",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sink2 := &ChannelSink{appId: "myApp2",
				identifier: "myAppChan2",
				done:       make(chan struct{}),
			}

			sinkManager.RegisterSink(sink1)
			sinkManager.RegisterSink(sink2)

			expectedMessageString := "Some Data"
			expectedMessage := NewMessage(expectedMessageString, "myApp")
			go sinkManager.SendTo("myApp1", expectedMessage)

			Eventually(sink1.Received).Should(HaveLen(1))
			Expect(sink1.Received()[0]).To(Equal(expectedMessage))
			Expect(sink2.Received()).To(BeEmpty())

			close(done)
		})
	})

	Describe("Stop", func() {

		It("should stop", func() {
			sinkManager.Stop()
			Eventually(sinkManagerDone).Should(BeClosed())
		})

		It("should stop all registered sinks", func(done Done) {
			sink := &ChannelSink{appId: "myApp1",
				identifier: "myAppChan1",
				done:       make(chan struct{}),
			}
			sinkManager.RegisterSink(sink)
			sinkManager.Stop()
			Expect(sink.RunFinished()).To(BeTrue())

			close(done)
		})
	})

	Context("With updates from appstore", func() {

		var metrics *metrics.SinkManagerMetrics
		var numSyslogSinks func() int

		BeforeEach(func() {
			metrics = sinkManager.Metrics
			numSyslogSinks = func() int {
				metrics.RLock()
				defer metrics.RUnlock()
				return metrics.SyslogSinks
			}
		})

		Context("When an add update is received", func() {
			It("Should create a new syslog sink from the newAppServicesChan", func() {
				initialNumSinks := numSyslogSinks()
				newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:885"}

				Eventually(numSyslogSinks).Should(Equal(initialNumSinks + 1))
			})

			Context("With an invalid drain Url", func() {
				var errorSink *ChannelSink

				BeforeEach(func() {
					errorSink = &ChannelSink{appId: "aptastic",
						identifier: "myAppChan1",
						done:       make(chan struct{}),
					}
					sinkManager.RegisterSink(errorSink)
				})

				It("Should send an error message if the drain URL is blacklisted", func() {
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://10.10.10.11:884"}
					Eventually(errorSink.Received).Should(HaveLen(1))
					errorMsg := errorSink.Received()[0]
					Expect(string(errorMsg.GetLogMessage().GetMessage())).To(MatchRegexp("Invalid syslog drain URL"))
				})

				It("Should send an error message if the drain URL is blacklisted", func() {
					newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog//invalid"}
					Eventually(errorSink.Received).Should(HaveLen(1))
					errorMsg := errorSink.Received()[0]
					Expect(string(errorMsg.GetLogMessage().GetMessage())).To(MatchRegexp("Invalid syslog drain URL"))
				})

			})

		})

		Context("When a delete update is received", func() {
			It("Should delete the corresponding syslog sink if it exists", func() {
				initialNumSinks := numSyslogSinks()
				newAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}

				Eventually(numSyslogSinks).Should(Equal(initialNumSinks + 1))

				deletedAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}

				Eventually(numSyslogSinks).Should(Equal(initialNumSinks))
			})

			It("Should handle a delete for a nonexistent sink", func() {
				initialNumSinks := numSyslogSinks()
				deletedAppServiceChan <- appservice.AppService{AppId: "aptastic", Url: "syslog://127.0.1.1:886"}
				Eventually(numSyslogSinks).Should(Equal(initialNumSinks))
			})

		})

	})

	Context("When a dump sink times out", func() {

		BeforeEach(func() {
			newAppServiceChan <- appservice.AppService{AppId: "appId", Url: "syslog://127.0.1.1:887"}
		})

		It("should remove the app from etcd", func(done Done) {
			dumpSink := dump.NewDumpSink("appId", 1, loggertesthelper.Logger(), 1*time.Millisecond)
			sinkManager.RegisterSink(dumpSink)

			Expect(<-appServicesChan).To(Equal(appservice.AppServices{AppId: "appId"}))
			close(done)
		})

	})

	Describe("UnregisterSink", func() {
		Context("with a DumpSink", func() {
			var dumpSink *dump.DumpSink

			BeforeEach(func() {
				dumpSink = dump.NewDumpSink("appId", 1, loggertesthelper.Logger(), time.Hour)
				sinkManager.RegisterSink(dumpSink)
			})

			It("clears the recent logs buffer", func() {
				expectedMessageString := "Some Data"
				expectedMessage := NewMessage(expectedMessageString, "appId")
				sinkManager.SendTo("appId", expectedMessage)

				Eventually(func() []*logmessage.Message {
					return sinkManager.RecentLogsFor("appId")
				}).Should(HaveLen(1))

				sinkManager.UnregisterSink(dumpSink)

				Eventually(func() []*logmessage.Message {
					return sinkManager.RecentLogsFor("appId")
				}).Should(HaveLen(0))
			})
		})

		Context("with a SyslogSink", func() {
			var syslogSink sinks.Sink

			BeforeEach(func() {
				url, err := url.Parse("syslog://localhost:9998")
				Expect(err).To(BeNil())
				writer := syslogwriter.NewSyslogWriter(url, "appId", true)
				errorChan := make(chan *logmessage.Message)
				syslogSink = syslog.NewSyslogSink("appId", "localhost:9999", loggertesthelper.Logger(), writer, errorChan)

				sinkManager.RegisterSink(syslogSink)
			})

			It("removes the sink", func() {
				Expect(sinkManager.Metrics.SyslogSinks).To(Equal(1))

				sinkManager.UnregisterSink(syslogSink)

				Expect(sinkManager.Metrics.SyslogSinks).To(Equal(0))
			})
		})

		Context("when called twice", func() {
			var dumpSink *dump.DumpSink

			BeforeEach(func() {
				dumpSink = dump.NewDumpSink("appId", 1, loggertesthelper.Logger(), time.Hour)
				sinkManager.RegisterSink(dumpSink)
			})

			It("decrements the metric only once", func() {
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(1))
				sinkManager.UnregisterSink(dumpSink)
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(0))
				sinkManager.UnregisterSink(dumpSink)
				Expect(sinkManager.Metrics.DumpSinks).To(Equal(0))
			})
		})
	})
})
