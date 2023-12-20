package nautobot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gosimple/slug"
	"github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/ctree"
	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	prom "github.com/prometheus/client_golang/prometheus"

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

func (e *NautobotExporter) Export(leaf *ctree.Leaf) {
	notification := leaf.Value().(*gnmipb.Notification)
	fmt.Println(notification)
	for _, update := range notification.Update {
		_, isNumber := utils.GetNumberValues(update.Val)
		if isNumber {
			continue
		}
		// fmt.Println(notification)
		metricName, labels := UpdateToMetricNameAndLabels(notification.GetPrefix(), update)

		device_slug, err := getSlug(labels, "target")
		if err != nil {
			fmt.Println(err)
			continue
		}
		interface_slug, err := getSlug(labels, "interfaces_interface_name")
		if err != nil {
			// fmt.Println(err)
			continue
		}
		fmt.Println(notification)

		endpoint := fmt.Sprintf("%s__%s", device_slug, interface_slug)
		interfaceStatus, ok := utils.GetStringValues(update.Val)
		if !ok {
			// fmt.Println(interfaceStatus)
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
				fmt.Println("oper: ", interfaceStatus)
			}
		case "interfaces_interface_state_admin_status":
			{
				if !exists {
					e.interaceStatus[endpoint] = map[string]string{}
					e.interaceStatus[endpoint]["admin"] = interfaceStatus
				} else {
					e.interaceStatus[endpoint]["admin"] = interfaceStatus
				}
				fmt.Println("admin: ", interfaceStatus)
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
	return nil
}

func UpdateToMetricNameAndLabels(prefix *gnmipb.Path, update *gnmipb.Update) (string, map[string]string) {
	metricName := ""
	labels := make(map[string]string)

	if prefix != nil {
		target := prefix.GetTarget()
		if target != "" {
			labels["target"] = target
		}
	}

	for _, elem := range update.Path.Elem {
		elemName := strings.ReplaceAll(elem.Name, "-", "_")
		if metricName == "" {
			metricName = elemName
		} else {
			metricName = metricName + "_" + elemName
		}
		for key, value := range elem.Key {
			labelKey := metricName + "_" + strings.ReplaceAll(key, "-", "_")
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
