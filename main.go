// New Relic Firehse Nozzle
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"runtime"

	"github.com/cloudfoundry-incubator/uaago"
	"github.com/cloudfoundry/noaa/consumer"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/kelseyhightower/envconfig"

	"github.com/cloudfoundry-community/go-cfclient"
	insights "github.com/newrelic/go-insights/client"
)

const (
	maxConnectionAttempts = 3
	pcfEventType          = "PcfFirehoseEvent"
)

type NewRelicConfig struct {
	INSIGHTS_BASE_URL   string
	INSIGHTS_RPM_ID     string
	INSIGHTS_INSERT_KEY string
}

type PcfExtConfig struct {
	GLOBAL_DEPLOYMENT_EXCLUSION_FILTERS string
	GLOBAL_ORIGIN_EXCLUSION_FILTERS     string
	GLOBAL_JOB_EXCLUSION_FILTERS        string

	VALUEMETRIC_DEPLOYMENT_INCLUSION_FILTERS string
	VALUEMETRIC_ORIGIN_INCLUSION_FILTERS     string
	VALUEMETRIC_JOB_INCLUSION_FILTERS        string
	VALUEMETRIC_METRIC_INCLUSION_FILTERS     string

	ADMIN_USER          string
	ADMIN_PASSWORD      string
	APP_DETAIL_INTERVAL string
	// REDIS_HOST          string
	// REDIS_PORT          string
	// REDIS_PASSWORD      string
    REDIS_DB            string
}

type ValueMetricFilterStruct struct {
	// GUID      string `json:"guid"`
	Value string `json:"value"`
}

type NREventType map[string]interface{}

type PcfCounters struct {
	valueMetricEvents   uint64
	counterEvents       uint64
	containerEvents     uint64
	httpStartStopEvents uint64
	logMessageEvents    uint64
	errors              uint64
}

var ee uint64
var pcfCounters PcfCounters
var mem runtime.MemStats
var nozzleInstanceIp string
var pcfDomain string
var appManager *AppManager
var insightsClient *insights.InsertClient

var NREventsMap = make([]NREventType, 0)
var nozzleInstanceId = os.Getenv("CF_INSTANCE_INDEX")

// var logger           = log.New(os.Stdout, ">>> ", 0)
var logger = log.New(os.Stdout, fmt.Sprintf(">>> Nozzle Instance: %3s -- ", nozzleInstanceId), 0)

var nozzleVersion string
var insightsMaxEvents int

var debug = false
var cfclientRefreshInterval = 58 // minutes to refresh go-cfclient credentials

type EventFilters struct {
	// global filters = exclusion
	globalDeploymentFilters              []string
	globalOriginFilters                  []string
	globalJobFilters                     []string
	globalAllFiltersSelected             bool
	globalDeploymentsAllFilterIsSelected bool
	globalOriginsAllFilterIsSelected     bool
	globalJobsAllFilterIsSelected        bool
	globalNoneSelected                   bool
	totalGlobalFiltersCount              int

	// value metric filters = inclusion
	valueMetricDeploymentFilters              []string
	valueMetricOriginFilters                  []string
	valueMetricJobFilters                     []string
	valueMetricMetricFilters                  []string
	valueMetricAllFiltersSelected             bool
	valueMetricDeploymentsAllFilterIsSelected bool
	valueMetricOriginsAllFilterIsSelected     bool
	valueMetricJobsAllFilterIsSelected        bool
	valueMetricMetricAllFilterIsSelected      bool
	valueMetricNoneSelected                   bool
	totalValueMetricFiltersCount              int
	totalValueMetricMetricsCount              int
}

type Config struct {
	APIURL                 string `envconfig:"api_url"`
	UAAURL                 string `envconfig:"uaa_url"`
	Username               string `required:"true"`
	Password               string `required:"true"`
	TrafficControllerURL   string `envconfig:"traffic_controller_url"`
	FirehoseSubscriptionID string `required:"true" envconfig:"firehose_subscription_id"`
	SkipSSL                bool   `default:"false" envconfig:"skip_ssl"`

	SelectedEvents []events.Envelope_EventType `ignored:"true"`
}

var filters EventFilters

var client *cfclient.Client
var cfClientErr error

func main() {
	logger.Println("New Relic Firehose Nozzle")
	if os.Getenv("DEBUG") == "1" || os.Getenv("DEBUG") == "true" || os.Getenv("DEBUG") == "TRUE" {
		debug = true
	}
	startHealthCheck()
	// ------------------------------------------------------------------------
	pcfConfig, err := Parse()
	if err != nil {
		panic(err)
	}

	if debug {
		logger.Printf("pcfConfig: %v\n", pcfConfig)
	}
	// ------------------------------------------------------------------------
	nrConfig := NewRelicConfig{}
	if err := envconfig.Process("newrelic", &nrConfig); err != nil {
		panic(err)
	}
	if debug {
		logger.Printf("nrConfig: %v\n", nrConfig)
	}
	// ------------------------------------------------------------------------
	pcfExtendedConfig := PcfExtConfig{}
	if err := envconfig.Process("NOZZLE", &pcfExtendedConfig); err != nil {
		panic(err)
	}
	if debug {
		logger.Printf("pcfExtendedConfig: %v\n", pcfExtendedConfig)
	}
	logger.Printf("ADMIN_USER: %s\n", pcfExtendedConfig.ADMIN_USER)
	if debug {
		logger.Printf("ADMIN_PASSWORD: %s\n", pcfExtendedConfig.ADMIN_PASSWORD)
	}
	logger.Printf("APP_DETAIL_INTERVAL: %s\n", pcfExtendedConfig.APP_DETAIL_INTERVAL)
	// ------------------------------------------------------------------------

	// ###########################################################################

	insightsUrl := fmt.Sprintf("%s/accounts", nrConfig.INSIGHTS_BASE_URL) // SK - 8-21-19 -- let go-insights build the full url
	insightsInsertKey := nrConfig.INSIGHTS_INSERT_KEY
	nozzleVersion = os.Getenv("NEWRELIC_NOZZLE_VERSION")
	insightsMaxEvents, err = strconv.Atoi(os.Getenv("NEWRELIC_INSIGHTS_MAX_EVENTS"))
	if err != nil {
		panic(err)
	}

	insightsClient = insights.NewInsertClient(insightsInsertKey, nrConfig.INSIGHTS_RPM_ID)
	if err := insightsClient.Validate(); err != nil {
		panic(err)
	}
	insightsClient.UseCustomURL(insightsUrl) // SK - 8-21-19 -- override with insights url from the nozzle config
	insightsClient.SetCompression(insights.Gzip)
	insightsClient.BatchSize = insightsMaxEvents
	err = insightsClient.Start()
	if err != nil {
		panic(err)
	}

	if debug {
		logger.Printf("insights url: %v\n", insightsUrl)
		logger.Printf("insights InsertKey: %v\n", insightsInsertKey)
	}
	logger.Printf("pcfConfig.SkipSSL: %v\n", pcfConfig.SkipSSL)
	if debug {
		logger.Printf("pcfConfig.APIURL: %v\n", pcfConfig.APIURL)
	}
	logger.Printf("pcfConfig.UAAURL: %v\n", pcfConfig.UAAURL)
	logger.Printf("pcfConfig.Username: %v\n", pcfConfig.Username)
	if debug {
		logger.Printf("pcfConfig.Password: %v\n", pcfConfig.Password)
	}
	nozzleInstanceIp = os.Getenv("CF_INSTANCE_IP")
	logger.Printf("Nozzle's CF_INSTANCE_IP: %v\n", nozzleInstanceIp)
	pcfDomain = strings.SplitN(parseUrl(pcfConfig.UAAURL), ".", 2)[1]
	logger.Printf("PCF Domain: %v\n", pcfDomain)
	logger.Printf("pcfConfig.SelectedEvents: %v\n", pcfConfig.SelectedEvents)

	setFilters(pcfExtendedConfig) // , filters) // sets all inclusion & exclusion filters in filter struct

	// os.Exit(0) // TEMP ################################################################
	// TEMP ###########################################################################

	includedEventTypes := map[events.Envelope_EventType]bool{
		events.Envelope_HttpStartStop:   false,
		events.Envelope_LogMessage:      false,
		events.Envelope_ValueMetric:     false,
		events.Envelope_CounterEvent:    false,
		events.Envelope_Error:           false,
		events.Envelope_ContainerMetric: false,
	}

	for _, selectedEventType := range pcfConfig.SelectedEvents {
		includedEventTypes[selectedEventType] = true
	}

	// authenticate client
	var token, trafficControllerURL string

	if pcfConfig.UAAURL != "" {
		logger.Printf("Fetching auth token via UAA: %v\n", pcfConfig.UAAURL)

		trafficControllerURL = pcfConfig.TrafficControllerURL
		if trafficControllerURL == "" {
			logger.Fatal(errors.New("NOZZLE_TRAFFIC_CONTROLLER_URL is required when authenticating via UAA"))
		}

		uaaClient, err := uaago.NewClient(pcfConfig.UAAURL)
		if err != nil {
			logger.Printf("Failed to create uaa client")
			logger.Fatal(err)
		}
		token, err = uaaClient.GetAuthToken(pcfConfig.Username, pcfConfig.Password, pcfConfig.SkipSSL)

		// fetcher := uaa.NewUAATokenFetcher(pcfConfig.UAAURL, pcfConfig.Username, pcfConfig.Password, pcfConfig.SkipSSL)
		// token, err = fetcher.FetchAuthToken()
		if err != nil {
			logger.Fatal("Unable to fetch token via UAA", err)
		}
	} else {
		logger.Fatal(errors.New("Either of NOZZLE_API_URL or NOZZLE_UAA_URL are required"))
	}
	// logger.Printf("token: %v\n", token)

	// ------------------------------------------
	// prepare to collect application details for ContainerEvent (app, space, org names, etc.)
	c := &cfclient.Config{
		ApiAddress:        "https://api." + pcfDomain,
		Username:          pcfExtendedConfig.ADMIN_USER,
		Password:          pcfExtendedConfig.ADMIN_PASSWORD,
		SkipSslValidation: pcfConfig.SkipSSL,
	}
	client, cfClientErr = cfclient.NewClient(c)
	if cfClientErr != nil {
		panic(cfClientErr)
	}

	//refreshClient causes a data race
	//TODO -- add capability to pass client safely to the appManager
	//refreshCfClient(c) // start goroutine to refresh cfclient credentials
	// ------------------------------------------

	appDetailsInterval, err := strconv.Atoi(pcfExtendedConfig.APP_DETAIL_INTERVAL)
	if err != nil {
		panic(err)
	}

	//start AppManager to managge application data
	appManager = NewAppManager(client, appDetailsInterval, &pcfExtendedConfig)
	appManager.Start()

	// consume PCF logs
	consumer := consumer.New(pcfConfig.TrafficControllerURL, &tls.Config{
		InsecureSkipVerify: pcfConfig.SkipSSL,
	}, nil)

	evs, errors := consumer.Firehose(pcfConfig.FirehoseSubscriptionID, token)
	ticker := time.NewTicker(time.Minute)

	i := 0
	logger.Printf("starting to capture firehose events...\n")
	for {
		select {
		case ev := <-evs:
			// logger.Printf("event %d: %v\n", i, ev)
			i++
			if (ev == nil) {
				logger.Println("Skipping null event")
			} else {
				firehoseEventType := ev.GetEventType()
				if includedEventTypes[firehoseEventType] {
					nrEvent := make(NREventType)
					if err := transformEvent(ev, nrEvent, pcfExtendedConfig, firehoseEventType.String()); err != nil {
						// event skipped -- do not insert
						logger.Printf("Skipped event: %v --- eve: %v\n", err.Error(), ev)
					} else { // insert event to insgihts
						//logger.Printf(">>reported: origin=%s  --  job=%s\n", ev.GetOrigin(), ev.GetJob())
						nrEvent["firehoseSubscriptionId"] = pcfConfig.FirehoseSubscriptionID
						nrEvent["nozzleVersion"] = nozzleVersion
						insightsClient.EnqueueEvent(nrEvent)
					}
				}
			}

		case ev := <-errors:
			logger.Println(i, " error: ", ev)
			//nrEvent["error"] = ev.Error()

		case <-ticker.C:
			logger.Printf("Value Metrics: %d, Counter Events: %d, Container Events: %d, Http StartStop Events: %d, Log Messages: %d, Errors: %d, Events Received in last minute: %d\n",
				pcfCounters.valueMetricEvents, pcfCounters.counterEvents, pcfCounters.containerEvents,
				pcfCounters.httpStartStopEvents, pcfCounters.logMessageEvents, pcfCounters.errors, i)
			i = 0

		}
	}
}

func getFilterValues(jsonFilters string) []string {

	valueMetricFilters := make([]ValueMetricFilterStruct, 0)
	json.Unmarshal([]byte(jsonFilters), &valueMetricFilters)
	valuesSlice := ""
	for k, v := range valueMetricFilters {
		if k == 0 {
			valuesSlice = v.Value
		} else {
			valuesSlice += "," + v.Value
		}
	}
	return splitString(valuesSlice, ",")
}

func filtered(arr []string, str string) bool {
	for _, s := range arr {
		if s == str {
			return true
		}
	}
	return false
}

func setFilters(pcfExtendedConfig PcfExtConfig) { //, filters EventFilters) {

	var allGlobalFilters []string
	var allValueMetricFilters []string

	filters.globalDeploymentFilters = splitString(pcfExtendedConfig.GLOBAL_DEPLOYMENT_EXCLUSION_FILTERS, ",")
	filters.globalOriginFilters = getFilterValues(pcfExtendedConfig.GLOBAL_ORIGIN_EXCLUSION_FILTERS)
	filters.globalJobFilters = getFilterValues(pcfExtendedConfig.GLOBAL_JOB_EXCLUSION_FILTERS)

	logger.Println("Global deployments filter: ", filters.globalDeploymentFilters)
	logger.Println("Global origins filter: ", filters.globalOriginFilters)
	logger.Println("Global jobs filter: ", filters.globalJobFilters)
	filters.totalGlobalFiltersCount = len(filters.globalDeploymentFilters) + len(filters.globalOriginFilters) + len(filters.globalJobFilters)
	logger.Printf("Global Filters count:: deployments: %d -- origins: %d -- jobs: %d\n", len(filters.globalDeploymentFilters), len(filters.globalOriginFilters), len(filters.globalJobFilters))

	filters.globalAllFiltersSelected = false
	filters.globalNoneSelected = false
	allGlobalFilters = append(allGlobalFilters, filters.globalDeploymentFilters...)
	allGlobalFilters = append(allGlobalFilters, filters.globalOriginFilters...)
	allGlobalFilters = append(allGlobalFilters, filters.globalJobFilters...)
	if filtered(allGlobalFilters, "all") {
		filters.globalAllFiltersSelected = true
	}
	// if filtered(allGlobalFilters, "none") {
	// 	filters.globalNoneSelected = true
	// }
	if filtered(filters.globalDeploymentFilters, "all") {
		filters.globalDeploymentsAllFilterIsSelected = true
	}
	if filtered(filters.globalOriginFilters, "all") {
		filters.globalOriginsAllFilterIsSelected = true
	}
	if filtered(filters.globalJobFilters, "all") {
		filters.globalJobsAllFilterIsSelected = true
	}

	filters.valueMetricDeploymentFilters = splitString(pcfExtendedConfig.VALUEMETRIC_DEPLOYMENT_INCLUSION_FILTERS, ",")
	filters.valueMetricOriginFilters = getFilterValues(pcfExtendedConfig.VALUEMETRIC_ORIGIN_INCLUSION_FILTERS)
	filters.valueMetricJobFilters = getFilterValues(pcfExtendedConfig.VALUEMETRIC_JOB_INCLUSION_FILTERS)
	filters.valueMetricMetricFilters = getFilterValues(pcfExtendedConfig.VALUEMETRIC_METRIC_INCLUSION_FILTERS)

	logger.Println("valueMetric deployments filter: ", filters.valueMetricDeploymentFilters)
	logger.Println("valueMetric origins filter: ", filters.valueMetricOriginFilters)
	logger.Println("valueMetric jobs filter: ", filters.valueMetricJobFilters)
	filters.totalValueMetricFiltersCount = len(filters.valueMetricDeploymentFilters) + len(filters.valueMetricOriginFilters) + len(filters.valueMetricJobFilters)
	logger.Println("valueMetric metric filter: ", filters.valueMetricMetricFilters)
	filters.totalValueMetricMetricsCount = len(filters.valueMetricMetricFilters)
	logger.Printf("Value Metric Filters count:: deployments: %d -- origins: %d -- jobs: %d -- metrics: %d\n", len(filters.valueMetricDeploymentFilters), len(filters.valueMetricOriginFilters), len(filters.valueMetricJobFilters), len(filters.valueMetricMetricFilters))

	filters.valueMetricAllFiltersSelected = false
	filters.valueMetricNoneSelected = false
	allValueMetricFilters = append(allValueMetricFilters, filters.valueMetricDeploymentFilters...)
	allValueMetricFilters = append(allValueMetricFilters, filters.valueMetricOriginFilters...)
	allValueMetricFilters = append(allValueMetricFilters, filters.valueMetricJobFilters...)
	if filtered(allValueMetricFilters, "all") {
		filters.valueMetricAllFiltersSelected = true
	}
	// if filtered(allValueMetricFilters, "none") {
	// 	filters.valueMetricNoneSelected = true
	// }
	if filtered(filters.valueMetricDeploymentFilters, "all") {
		filters.valueMetricDeploymentsAllFilterIsSelected = true
	}
	if filtered(filters.valueMetricOriginFilters, "all") {
		filters.valueMetricOriginsAllFilterIsSelected = true
	}
	if filtered(filters.valueMetricJobFilters, "all") {
		filters.valueMetricJobsAllFilterIsSelected = true
	}
	if filtered(filters.valueMetricMetricFilters, "all") {
		filters.valueMetricMetricAllFilterIsSelected = true
	}

	logger.Printf("filters.globalDeploymentFilters: exclude %v\n", filters.globalDeploymentFilters)
	logger.Printf("filters.globalOriginFilters: exclude %v\n", filters.globalOriginFilters)
	logger.Printf("filters.globalJobFilters: exclude %v\n", filters.globalJobFilters)
	logger.Printf("filters.globalAllFiltersSelected: %v\n", filters.globalAllFiltersSelected)
	logger.Printf("filters.globalDeploymentsAllFilterIsSelected: %v\n", filters.globalDeploymentsAllFilterIsSelected)
	logger.Printf("filters.globalOriginsAllFilterIsSelected: %v\n", filters.globalOriginsAllFilterIsSelected)
	logger.Printf("filters.globalJobsAllFilterIsSelected: %v\n", filters.globalJobsAllFilterIsSelected)
	logger.Printf("filters.globalNoneSelected: %v\n", filters.globalNoneSelected)
	logger.Printf("filters.totalGlobalFiltersCount: %v\n", filters.totalGlobalFiltersCount)

	logger.Printf("filters.valueMetricDeploymentFilters: include %v\n", filters.valueMetricDeploymentFilters)
	logger.Printf("filters.valueMetricOriginFilters: include %v\n", filters.valueMetricOriginFilters)
	logger.Printf("filters.valueMetricJobFilters: include %v\n", filters.valueMetricJobFilters)
	logger.Printf("filters.valueMetricMetricFilters: include %v\n", filters.valueMetricMetricFilters)
	logger.Printf("filters.valueMetricAllFiltersSelected: %v\n", filters.valueMetricAllFiltersSelected)
	logger.Printf("filters.valueMetricDeploymentsAllFilterIsSelected: %v\n", filters.valueMetricDeploymentsAllFilterIsSelected)
	logger.Printf("filters.valueMetricOriginsAllFilterIsSelected: %v\n", filters.valueMetricOriginsAllFilterIsSelected)
	logger.Printf("filters.valueMetricJobsAllFilterIsSelected: %v\n", filters.valueMetricJobsAllFilterIsSelected)
	logger.Printf("filters.valueMetricMetricAllFilterIsSelected: %v\n", filters.valueMetricMetricAllFilterIsSelected)
	logger.Printf("filters.valueMetricNoneSelected: %v\n", filters.valueMetricNoneSelected)
	logger.Printf("filters.totalValueMetricFiltersCount: %v\n", filters.totalValueMetricFiltersCount)
	logger.Printf("filters.totalValueMetricMetricsCount: %v\n", filters.totalValueMetricMetricsCount)
}

func refreshCfClient(c *cfclient.Config) {
	logger.Printf("Starting Goroutine refreshCfClient -- refreshing go-cfclient credentials every %d minute(s)\n", cfclientRefreshInterval)
	ticker := time.NewTicker(time.Duration(int64(cfclientRefreshInterval)) * time.Minute)
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				logger.Print("refreshing cfclient...\r\n")
				client, cfClientErr = cfclient.NewClient(c)
				if cfClientErr != nil {
					panic(cfClientErr)
				}
				if debug {
					fmt.Printf("c: %v\n", c)
					fmt.Printf("client: %v\n", client)
				}

			case <-quit:
				logger.Print("quit \r\n")
				ticker.Stop()
			}
		}
	}()
}

func fillGenericMetrics(nrEvent map[string]interface{}, eventOrigin string, firehoseEventType string,
	eventDeployment string, eventJob string, eventIndex string,
	eventIp string, eventTimestamp int64, tags map[string]string) {
	// add generic fields
	nrEvent["origin"] = eventOrigin
	nrEvent["eventType"] = pcfEventType
	nrEvent["FirehoseEventType"] = firehoseEventType
	if pcfDomain > "" {
		nrEvent["pcfDomain"] = pcfDomain
	}
	if nozzleInstanceIp > "" {
		nrEvent["nozzleInstanceIp"] = nozzleInstanceIp
	}
	nrEvent["nozzleInstanceId"] = nozzleInstanceId
	nrEvent["deployment"] = eventDeployment
	nrEvent["job"] = eventJob
	nrEvent["index"] = eventIndex
	nrEvent["ip"] = eventIp
	nrEvent["timestamp"] = eventTimestamp / 1000000 // nanoseconds -> milliseconds
	for name, val := range tags {
		nrEvent["tag_"+name] = val
	}
}

func transformEvent(cfEvent *events.Envelope, nrEvent map[string]interface{}, pcfExtendedConfig PcfExtConfig, firehoseEventType string) error {

	eventDeployment := cfEvent.GetDeployment()
	eventOrigin := cfEvent.GetOrigin()
	eventJob := cfEvent.GetJob()

	// global event exclusion filters
	globalEventsFilter := false
	processEvent := true
	if filters.totalGlobalFiltersCount > 0 && (filters.globalAllFiltersSelected || filtered(filters.globalDeploymentFilters, eventDeployment) || filtered(filters.globalOriginFilters, eventOrigin) || filtered(filters.globalJobFilters, eventJob)) {
		globalEventsFilter = true
		processEvent = false
	}

	// TODO
	// {
	//	break wown filters for deployments, origins, and jobs --
	//	and use each with corresponding filter in ValueMetric events

	// 	globalDeploymentsExclusionFilterIsSet := false
	// 	globalOriginsExclusionFilterIsSet     := false
	// 	globalDJobsExclusionFilterIsSet       := false
	// /*
	// 	filters.globalDeploymentsAllFilterIsSelected
	// 	filters.globalOriginsAllFilterIsSelected
	// 	filters.globalJobsAllFilterIsSelected

	// */
	// 	if (filters.globalDeploymentsAllFilterIsSelected || filtered(filters.globalDeploymentFilters, eventDeployment)) {
	// 		globalDeploymentsExclusionFilterIsSet = true
	// 	}

	// 	if (filters.globalOriginsAllFilterIsSelected || filtered(filters.globalOriginFilters, eventOrigin)) {
	// 		globalOriginsExclusionFilterIsSet = true
	// 	}

	// 	if (filters.globalJobsAllFilterIsSelected || filtered(filters.globalJobFilters, eventJob)) {
	// 		globalDJobsExclusionFilterIsSet = true
	// 	}

	// 	// if any of global filters is set
	// 	if (firehoseEventType == "ValueMetric" && (globalDeploymentsExclusionFilterIsSet || globalOriginsExclusionFilterIsSet || globalDJobsExclusionFilterIsSet)) {
	// 		// then check to see if there is a need to
	// 	}
	// }

	// valuemetric event inclusion filters
	if globalEventsFilter == false { // no match found for exclusion filter at globel level
		processEvent = true
	} else { // globalEventsFilter == true -- match found for exclusion filters at globel level
		if firehoseEventType == "ValueMetric" {
			if (filters.totalValueMetricFiltersCount + filters.totalValueMetricMetricsCount) == 0 { // no inclusion filter selected
				processEvent = false
			} else {
				vmName := cfEvent.ValueMetric.GetName()
				if filters.valueMetricAllFiltersSelected || filtered(filters.valueMetricDeploymentFilters, eventDeployment) || filtered(filters.valueMetricOriginFilters, eventOrigin) || filtered(filters.valueMetricJobFilters, eventJob) || filtered(filters.valueMetricMetricFilters, vmName) {
					processEvent = true
				}
			}
		}
	}

	if processEvent {
		eventIndex := cfEvent.GetIndex() // returns index quid or ""
		eventIp := cfEvent.GetIp() // returns ip or ""
		eventTimestamp := cfEvent.GetTimestamp() // returns timestamp or 0
		eventTags := cfEvent.GetTags() // returns map[string]string of tags or nil
		fillGenericMetrics(nrEvent, eventOrigin, firehoseEventType, eventDeployment, eventJob,
			eventIndex, eventIp, eventTimestamp, eventTags)

		// // add generic fields
		// nrEvent["origin"] = eventOrigin
		// nrEvent["eventType"] = pcfEventType
		// nrEvent["FirehoseEventType"] = firehoseEventType
		// if pcfDomain > "" {
		// 	nrEvent["pcfDomain"] = pcfDomain
		// }
		// if nozzleInstanceIp > "" {
		// 	nrEvent["nozzleInstanceIp"] = nozzleInstanceIp
		// }
		// nrEvent["nozzleInstanceId"] = nozzleInstanceId
		// nrEvent["deployment"] = eventDeployment
		// nrEvent["job"] = eventJob
		// nrEvent["index"] = cfEvent.GetIndex()
		// nrEvent["ip"] = cfEvent.GetIp()
		// nrEvent["timestamp"] = cfEvent.GetTimestamp() / 1000000 // nanoseconds -> milliseconds

		// for name, val := range cfEvent.Tags {
		// 	nrEvent["tag_"+name] = val
		// }

		// get in to event type specific stuff
		switch *cfEvent.EventType {
		case events.Envelope_HttpStartStop:
			pcfCounters.httpStartStopEvents++
			transformHttpStartStopEvent(cfEvent, nrEvent)

		case events.Envelope_LogMessage:
			pcfCounters.logMessageEvents++
			transformLogMessage(cfEvent, nrEvent)

		case events.Envelope_ContainerMetric:
			pcfCounters.containerEvents++
			transformContainerMetric(cfEvent, nrEvent)
			//nrEvent["containerMetric"] = cfEvent.GetContainerMetric().String()

		case events.Envelope_CounterEvent:
			pcfCounters.counterEvents++
			transformCounterEvent(cfEvent, nrEvent)
			// nrEvent["counterEvent"] = cfEvent.GetCounterEvent().String()

		case events.Envelope_ValueMetric:

			pcfCounters.valueMetricEvents++
			transformValueMetric(cfEvent, nrEvent)
			//nrEvent["valueMetric"] = cfEvent.GetValueMetric().String()

		case events.Envelope_Error:
			pcfCounters.errors++
			transformErrorEvent(cfEvent, nrEvent)
			//nrEvent["errorField"] = cfEvent.GetError().String()
			if debug {
				logger.Println(">>> ERROR EVENT: " + cfEvent.GetError().String())
			}
		}
		return nil
	} else {
		return errors.New("eventskipped")
	}
}

// process ValueMetric events to new relic event format
func transformValueMetric(cfEvent *events.Envelope, nrEvent map[string]interface{}) {
	// event: origin:"DopplerServer" eventType:ValueMetric timestamp:1497038365914920486 deployment:"cf" job:"doppler" index:"ca858dc5-2a09-465a-831d-c31fa5fb8802" ip:"192.168.16.26" valueMetric:<name:"messageRouter.numberOfFirehoseSinks" value:1 unit:"sinks" >
	vm := cfEvent.ValueMetric
	prefix := "valueMetric"
	if vm.Name != nil {
		nrEvent[prefix+"Name"] = vm.GetName()
	}
	if vm.Value != nil {
		nrEvent[prefix+"Value"] = vm.GetValue()
	}
	if vm.Unit != nil {
		nrEvent[prefix+"Unit"] = vm.GetUnit()
	}
}

// process CounterEvent events to new relic event format
func transformCounterEvent(cfEvent *events.Envelope, nrEvent map[string]interface{}) {
	// event: origin:"DopplerServer" eventType:CounterEvent timestamp:1497038366107650076 deployment:"cf" job:"doppler" index:"ca858dc5-2a09-465a-831d-c31fa5fb8802" ip:"192.168.16.26" counterEvent:<name:"udpListener.receivedByteCount" delta:152887 total:25671098577 >
	ce := cfEvent.CounterEvent
	prefix := "counterEvent"
	if ce.Name != nil {
		nrEvent[prefix+"Name"] = ce.GetName()
	}
	if ce.Delta != nil {
		nrEvent[prefix+"Delta"] = ce.GetDelta()
	}
	if ce.Total != nil {
		nrEvent[prefix+"Total"] = ce.GetTotal()
	}
}

func addAppDetailInfo(nrEvent map[string]interface{}, appGUID string) {
	// add app detail info to insights event
	appData := appManager.GetAppData(appGUID)
	nrEvent["appInfoUpdated"] = appData.Timestamp
	nrEvent["appName"] = appData.Name
	nrEvent["appGuid"] = appData.Guid
	nrEvent["appCreated"] = appData.CreatedTime
	nrEvent["appLastUpdated"] = appData.LastUpdated
	nrEvent["appInstances"] = strconv.FormatInt(int64(appData.Instances), 10)
	nrEvent["appStackGuid"] = appData.StackGUID
	nrEvent["appState"] = appData.State
	nrEvent["diegoContainer"] = strconv.FormatBool(appData.Diego)
	nrEvent["appSshEnabled"] = strconv.FormatBool(appData.SshEnabled)
	nrEvent["appSpaceName"] = appData.SpaceName
	nrEvent["appSpaceGuid"] = appData.SpaceGUID
	nrEvent["appOrgName"] = appData.OrgName
	nrEvent["appOrgGuid"] = appData.OrgGUID
}

// process ContainerMetric events to new relic event format
func transformContainerMetric(cfEvent *events.Envelope, nrEvent map[string]interface{}) {
	// event: origin:"rep" eventType:ContainerMetric timestamp:1497038370673051301 deployment:"cf" job:"diego_cell" index:"302e37ef-f847-4b96-bdff-5c6e4f0d1259" ip:"192.168.16.23" containerMetric:<applicationId:"a0bc8fd4-8980-4e0e-81b3-7f9709ff407e" instanceIndex:0 cpuPercentage:0.07382914424191898 memoryBytes:359899136 diskBytes:142286848 memoryBytesQuota:536870912 diskBytesQuota:1073741824 >
	cm := cfEvent.ContainerMetric
	prefix := "containerMetric"
	if cm.ApplicationId != nil {
		appGuid := cm.GetApplicationId()
		nrEvent[prefix+"ApplicationId"] = appGuid //cm.GetApplicationId()
		addAppDetailInfo(nrEvent, appGuid)        // add app detail info to Insights ContainerMetric event
	}
	if cm.InstanceIndex != nil {
		nrEvent[prefix+"InstanceIndex"] = cm.GetInstanceIndex()
	}
	if cm.CpuPercentage != nil {
		nrEvent[prefix+"CpuPercentage"] = cm.GetCpuPercentage()
	}
	if cm.MemoryBytes != nil {
		nrEvent[prefix+"MemoryBytes"] = cm.GetMemoryBytes()
	}
	if cm.DiskBytes != nil {
		nrEvent[prefix+"DiskBytes"] = cm.GetDiskBytes()
	}
	if cm.MemoryBytesQuota != nil {
		nrEvent[prefix+"MemoryBytesQuota"] = cm.GetMemoryBytesQuota()
	}
	if cm.DiskBytesQuota != nil {
		nrEvent[prefix+"DiskBytesQuota"] = cm.GetDiskBytesQuota()
	}
}

// process application log events to new relic event format
func transformLogMessage(cfEvent *events.Envelope, nrEvent map[string]interface{}) {
	// event: origin:"rep" eventType:LogMessage timestamp:1497038366041617814 deployment:"cf" job:"diego_cell" index:"0f4dc7bd-c941-42bf-a835-7c29445ddf8b" ip:"192.168.16.24" logMessage:<message:"[{\"DatasetName\":\"Metric Messages\",\"FirehoseEventType\":\"CounterEvent\",\"ceDelta\":166908,\"ceName\":\"dropsondeListener.receivedByteCount\",\"ceTotal\":25664179951,\"deployment\":\"cf\",\"eventType\":\"FirehoseEventTest\",\"index\":\"ca858dc5-2a09-465a-831d-c31fa5fb8802\",\"ip\":\"192.168.16.26\",\"job\":\"doppler\",\"origin\":\"DopplerServer\",\"timestamp\":1497038161107}]" message_type:OUT timestamp:1497038366041615818 app_id:"f22aac70-c5a9-47a9-b74c-355dd99abbe2" source_type:"APP/PROC/WEB" source_instance:"0" >
	message := cfEvent.LogMessage
	// if debug {
	// 	logger.Printf(">>>>> raw log message: %v\n", cfEvent)
	// }
	prefix := "log"
	if message.Message != nil {
		msgContent := message.GetMessage()
		if len(msgContent) > 4096 {
			msgContent = msgContent[0:4095]
		}
		// re := regexp.MustCompile("=>")
		// payload := string([]byte(`payload: {"instance"=>"a305bf1e-f869-4307-5bdc-7f7b", "index"=>0, "reason"=>"CRASHED", "exit_description"=>"Instance never healthy after 1m0s: Failed to make TCP connection to port 8080: connection refused", "crash_count"=>2, "crash_timestamp"=>1522812923161363839, "version"=>"68a457a6-2f43-4ed7-af5f-038f2e1da1fc"}`))
		// fmt.Println(re.ReplaceAllString(payload, ": "))
		// logger.Printf(">>>>> log message payload: %v\n", string(msgContent))
		nrEvent[prefix+"Message"] = string(msgContent)
		parsedContent := make(map[string]interface{})
		if err := json.Unmarshal(msgContent, &parsedContent); err == nil {
			for k, v := range parsedContent {
				nrEvent[prefix+"Message"+k] = v
			}
		}
		addAppDetailInfo(nrEvent, message.GetAppId()) // add app detail info to Insights LogMessage event
	}
	if message.MessageType != nil {
		nrEvent[prefix+"MessageType"] = message.GetMessageType().String()
	}
	if message.Timestamp != nil {
		nrEvent[prefix+"Timestamp"] = time.Unix(0, message.GetTimestamp())
	}
	if message.AppId != nil {
		nrEvent[prefix+"AppId"] = message.GetAppId()
	}
	if message.SourceType != nil {
		nrEvent[prefix+"SourceType"] = message.GetSourceType()
	}
	if message.SourceInstance != nil {
		nrEvent[prefix+"SourceInstance"] = message.GetSourceInstance()
	}
}

// process http start/stop events to new relic event format
func transformHttpStartStopEvent(cfEvent *events.Envelope, nrEvent map[string]interface{}) {
	// event: origin:"gorouter" eventType:HttpStartStop timestamp:1497038373295178447 deployment:"cf" job:"router" index:"1276dbaa-f5a4-4c48-bcbe-d06ff0dba58d" ip:"192.168.16.16" httpStartStop:<startTimestamp:1497038373206213992 stopTimestamp:1497038373295152451 requestId:<low:7513566559519661218 high:8828490834936076361 > peerType:Client method:GET uri:"http://api.sys.pie-22.cfplatformeng.com/v2/syslog_drain_urls" remoteAddress:"130.211.2.63:61939" userAgent:"Go-http-client/1.1" statusCode:200 contentLength:42 instanceId:"89a53ed9-cf20-404b-5728-33a19c1e13ef" forwarded:"104.197.98.14" forwarded:"35.186.215.103" forwarded:"130.211.2.63" >
	httpEvent := cfEvent.HttpStartStop
	prefix := "http"
	start := time.Unix(0, httpEvent.GetStartTimestamp())
	end := time.Unix(0, httpEvent.GetStopTimestamp())
	duration := float64(end.Sub(start)) / float64(time.Millisecond)
	nrEvent[prefix+"StartTimestamp"] = start
	nrEvent[prefix+"StopTimestamp"] = end
	nrEvent[prefix+"DurationMs"] = duration
	if httpEvent.RequestId != nil {
		nrEvent[prefix+"RequestId"] = httpEvent.GetRequestId().String()
	}
	if httpEvent.PeerType != nil {
		nrEvent[prefix+"PeerType"] = httpEvent.GetPeerType().String()
	}
	if httpEvent.Method != nil {
		nrEvent[prefix+"Method"] = httpEvent.GetMethod().String()
	}
	if httpEvent.Uri != nil {
		nrEvent[prefix+"Uri"] = httpEvent.GetUri()
	}
	if httpEvent.RemoteAddress != nil {
		nrEvent[prefix+"RemoteAddress"] = httpEvent.GetRemoteAddress()
	}
	if httpEvent.UserAgent != nil {
		nrEvent[prefix+"UserAgent"] = httpEvent.GetUserAgent()
	}
	if httpEvent.StatusCode != nil {
		nrEvent[prefix+"StatusCode"] = httpEvent.GetStatusCode()
	}
	if httpEvent.ContentLength != nil {
		nrEvent[prefix+"ContentLength"] = httpEvent.GetContentLength()
	}
	if httpEvent.ApplicationId != nil {
		nrEvent[prefix+"ApplicationId"] = httpEvent.GetApplicationId()
	}
	if httpEvent.InstanceIndex != nil {
		nrEvent[prefix+"InstanceIndex"] = httpEvent.GetInstanceIndex()
	}
	if httpEvent.InstanceId != nil {
		nrEvent[prefix+"InstanceId"] = httpEvent.GetInstanceId()
	}
	for i, forwardedIp := range httpEvent.Forwarded {
		index := strconv.Itoa(i)
		nrEvent[prefix+"Forwarded"+index] = forwardedIp
	}
}

// process Error events to new relic event format
func transformErrorEvent(cfEvent *events.Envelope, nrEvent map[string]interface{}) {

	errEvent := cfEvent.Error
	prefix := "error"
	if errEvent.Source != nil {
		nrEvent[prefix+"Source"] = errEvent.GetSource()
	}
	if errEvent.Code != nil {
		nrEvent[prefix+"Code"] = errEvent.GetCode()
	}
	if errEvent.Message != nil {
		nrEvent[prefix+"Message"] = errEvent.GetMessage()
	}
}

func checkMem(seq int) {
	runtime.ReadMemStats(&mem)
	log.Println(seq, ": allocated: ", mem.Alloc, " - total allocated: ", mem.TotalAlloc, " - heap allocated: ", mem.HeapAlloc, " - heap sys: ", mem.HeapSys)
}

func parseUrl(uaaUrl string) string {

	u, err := url.Parse(uaaUrl)
	if err != nil {
		panic(err)
	}

	return u.Host
}

func splitString(value string, separator string) []string {

	filters := []string{}
	if value == "" {
		filters = nil // no filter set
	} else {
		for _, envValueSplit := range strings.Split(value, separator) {
			filters = append(filters, strings.TrimSpace(envValueSplit))
		}
	}
	return filters
}

func startHealthCheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	go func() {
		http.HandleFunc("/health", healthCheckHandler)
		logger.Fatal(http.ListenAndServe(":"+port, nil))
	}()
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "I'm alive and well!")
}

func Parse() (*Config, error) {
	config := &Config{}

	err := envconfig.Process("nozzle", config)
	if err != nil {
		return nil, err
	}

	selectedEvents, err := parseSelectedEvents()
	if err != nil {
		return nil, err
	}
	config.SelectedEvents = selectedEvents

	return config, nil
}

func parseSelectedEvents() ([]events.Envelope_EventType, error) {
	envValue := os.Getenv("NOZZLE_SELECTED_EVENTS")
	if envValue == "" {
		var defaultEvents = []events.Envelope_EventType{
			events.Envelope_ValueMetric,
			events.Envelope_CounterEvent,
		}
		return defaultEvents, nil
	} else {
		selectedEvents := []events.Envelope_EventType{}

		for _, envValueSplit := range strings.Split(envValue, ",") {
			envValueSlitTrimmed := strings.TrimSpace(envValueSplit)
			val, found := events.Envelope_EventType_value[envValueSlitTrimmed]
			if found {
				selectedEvents = append(selectedEvents, events.Envelope_EventType(val))
			} else {
				return nil, errors.New(fmt.Sprintf("[%s] is not a valid event type", envValueSlitTrimmed))
			}
		}
		return selectedEvents, nil
	}
}
