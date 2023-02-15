package nautobot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"net/http"

	"github.com/gosimple/slug"
	"github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/ctree"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	// "github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mspiez/gnmi-gateway/gateway/configuration"
	"github.com/mspiez/gnmi-gateway/gateway/exporters"
	"github.com/mspiez/gnmi-gateway/gateway/openconfig"
	"github.com/mspiez/gnmi-gateway/gateway/utils"
	"github.com/mspiez/nautobot-go-client/nautobot"
)

const Name = "nautobot"

var _ exporters.Exporter = new(NautobotExporter)

func init() {
	exporters.Register(Name, NewNautobotExporter)
}

func NewNautobotExporter(config *configuration.GatewayConfig) exporters.Exporter {
	return &NautobotExporter{
		config:         config,
		deltaCalc:      NewDeltaCalculator(),
		metrics:        make(map[Hash]prom.Metric),
		interaceStatus: map[string]map[string]string{},
		typeLookup:     new(openconfig.TypeLookup),
	}
}

type NautobotExporter struct {
	config         *configuration.GatewayConfig
	cache          *cache.Cache
	deltaCalc      *DeltaCalculator
	metrics        map[Hash]prom.Metric
	interaceStatus map[string]map[string]string
	typeLookup     *openconfig.TypeLookup
}

func (e *NautobotExporter) Name() string {
	return Name
}

// timestamp:1666212263233342118
// prefix:{elem:{name:"interfaces"} elem:{name:"interface" key:{key:"name" value:"Management1"}}
// elem:{name:"state"} elem:{name:"counters"} target:"R2"}
// update:{path:{elem:{name:"out-pkts"}} val:{uint_val:3094}}
func (e *NautobotExporter) Export(leaf *ctree.Leaf) {
	notification := leaf.Value().(*gnmipb.Notification)
	fmt.Println("Notification: ", notification)
	for _, update := range notification.Update {
		value, isNumber := utils.GetNumberValues(update.Val)
		fmt.Println("Val, isNumber: ", value, isNumber)
		if isNumber {
			continue
		}
		metricName, labels := UpdateToMetricNameAndLabels(notification.GetPrefix(), update)
		metricHash := NewStringMapHash(metricName, labels)
		fmt.Println("metricName: ", metricName)
		fmt.Println("Labels: ", labels)
		fmt.Println("metricHash: ", metricHash)
		device_slug, err := getSlug(labels, "target")
		if err != nil {
			fmt.Println(err)
			continue
		}
		interface_slug, err := getSlug(labels, "interfaces_interface_name")
		if err != nil {
			fmt.Println(err)
			continue
		}

		endpoint := fmt.Sprintf("%s__%s", device_slug, interface_slug)
		interfaceStatus, ok := utils.GetStringValues(update.Val)
		if !ok {
			fmt.Println(ok)
			continue
		}

		_, exists := e.interaceStatus[endpoint]
		switch metricName {
		case "interfaces_interface_state_oper_status":
			{
				if !exists {
					e.interaceStatus[endpoint] = map[string]string{}
					e.interaceStatus[endpoint]["oper"] = interfaceStatus
				} else {
					e.interaceStatus[endpoint]["oper"] = interfaceStatus
				}
			}
		case "interfaces_interface_state_admin_status":
			{
				if !exists {
					e.interaceStatus[endpoint] = map[string]string{}
					e.interaceStatus[endpoint]["admin"] = interfaceStatus
				} else {
					e.interaceStatus[endpoint]["admin"] = interfaceStatus
				}
			}
		}

		reqBody := createRequestBody(labels, e.interaceStatus[endpoint])
		updated, err := UpdateInterfaceStatus(reqBody, endpoint)

		if err != nil {
			fmt.Println(err)
			continue
		}

		fmt.Println(updated)
	}
}

func (e *NautobotExporter) Start(cache *cache.Cache) error {
	e.config.Log.Info().Msg("Starting Nautobot exporter.")
	if e.config.OpenConfigDirectory == "" {
		return errors.New("value is not set for OpenConfigDirectory configuration")
	}
	e.cache = cache
	err := e.typeLookup.LoadAllModules(e.config.OpenConfigDirectory)
	if err != nil {
		e.config.Log.Error().Err(err).Msgf("Unable to load OpenConfig modules in %s: %v", e.config.OpenConfigDirectory, err)
		return err
	}
	// e.ReadEnvs()
	go e.runHttpServer()
	return nil
}

func (e *NautobotExporter) runHttpServer() {
	var errCount = 0
	var lastError error
	for {
		e.config.Log.Info().Msg("Starting Prometheus HTTP server.")
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(":59100", nil)
		if err != nil {
			e.config.Log.Error().Err(err).Msgf("Prometheus HTTP server stopped with an error: %v", err)
			if err.Error() == lastError.Error() {
				errCount = errCount + 1
				if errCount >= 3 {
					panic(fmt.Errorf("too many errors returned by Prometheus HTTP server: %s", err.Error()))
				}
			} else {
				errCount = 0
				lastError = err
			}
		}
	}
}

func UpdateToMetricNameAndLabels(prefix *gnmipb.Path, update *gnmipb.Update) (string, map[string]string) {
	metricName := ""
	labels := make(map[string]string)

	if prefix != nil {
		target := prefix.GetTarget()
		if target != "" {
			// R2
			labels["target"] = target
		}
	}

	for _, elem := range update.Path.Elem {
		// in-pkts => in_pkts
		elemName := strings.ReplaceAll(elem.Name, "-", "_")
		if metricName == "" {
			// metricName -> in_pkts
			metricName = elemName
		} else {
			// update:{path:{elem:{name:"meta"} elem:{name:"sync"}} val:{bool_val:true}}
			// meta_sync
			metricName = metricName + "_" + elemName
		}
		// elem:{name:"interface" key:{key:"name" value:"Management1"}}
		for key, value := range elem.Key {
			// metricName = in_pkts
			// labelKey -> in_pkts_name
			labelKey := metricName + "_" + strings.ReplaceAll(key, "-", "_")
			// in_pkts_name = "Management1"
			labels[labelKey] = value
		}
	}
	return metricName, labels
}

func getSlug(labels map[string]string, str string) (string, error) {
	val, ok := labels[str]
	if ok {
		slug := slug.Make(val)
		return slug, nil
	}
	return val, fmt.Errorf("No label: %v", str)
}

func createRequestBody(labels map[string]string, interfaceStatus map[string]string) map[string]string {
	requestBody := map[string]string{
		"device_name":    labels["target"],
		"interface_name": labels["interfaces_interface_name"],
	}
	admin_state, exists := interfaceStatus["admin"]
	if exists {
		requestBody["interface_admin_status"] = admin_state
	}
	oper_state, exists := interfaceStatus["oper"]
	if exists {
		requestBody["interface_oper_status"] = oper_state
	}
	return requestBody
}

func UpdateInterfaceStatus(reqBody map[string]string, endpoint string) (bool, error) {
	token := os.Getenv("NAUTOBOT_TOKEN")
	baseUrl := os.Getenv("NAUTOBOT_URL")

	ctx := context.Background()
	timeout := 30 * time.Second
	reqContext, _ := context.WithTimeout(ctx, timeout)

	newClient := nautobot.NewClient(
		nautobot.WithToken(token),
		nautobot.WithBaseURL(baseUrl),
	)

	_, err := newClient.PatchInterfaceStatus(reqContext, endpoint, reqBody)

	if err != nil {
		var requestError *nautobot.PatchRequestError
		if errors.As(err, &requestError) {
			fmt.Println(requestError.Message)
			if requestError.DetailNotFound {
				fmt.Println("Interface does not exists in Nautobot. Creating it...")
				newReqBody := make([]map[string]string, 1)
				newReqBody[0] = reqBody
				_, err := newClient.PostInterfacesStatus(reqContext, newReqBody)
				if err != nil {
					fmt.Println("Could not create Interface in Nautobot")
				}
				fmt.Println("Interface created in Nautobot")
			}
			return false, requestError
		}
		return false, err
	}
	fmt.Println("Interface updated in Nautobot")
	return true, nil
}
