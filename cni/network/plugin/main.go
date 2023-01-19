// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/Azure/azure-container-networking/aitelemetry"
	"github.com/Azure/azure-container-networking/cni"
	"github.com/Azure/azure-container-networking/cni/api"
	"github.com/Azure/azure-container-networking/cni/network"
	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/nns"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/Azure/azure-container-networking/store"
	"github.com/Azure/azure-container-networking/telemetry"
	"github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
)

const (
	hostNetAgentURL                 = "http://168.63.129.16/machine/plugins?comp=netagent&type=cnireport"
	ipamQueryURL                    = "http://168.63.129.16/machine/plugins?comp=nmagent&type=getinterfaceinfov1"
	pluginName                      = "CNI"
	telemetryNumRetries             = 5
	telemetryWaitTimeInMilliseconds = 200
	name                            = "azure-vnet"
)

// Version is populated by make during build.
var version string

// Command line arguments for CNI plugin.
var args = common.ArgumentList{
	{
		Name:         common.OptVersion,
		Shorthand:    common.OptVersionAlias,
		Description:  "Print version information",
		Type:         "bool",
		DefaultValue: false,
	},
}

// Prints version information.
func printVersion() {
	fmt.Printf("Azure CNI Version %v\n", version)
}

// send error report to hostnetagent if CNI encounters any error.
func reportPluginError(reportManager *telemetry.ReportManager, tb *telemetry.TelemetryBuffer, err error) {
	log.Printf("Report plugin error")
	reflect.ValueOf(reportManager.Report).Elem().FieldByName("ErrorMessage").SetString(err.Error())

	if err := reportManager.SendReport(tb); err != nil {
		log.Errorf("SendReport failed due to %v", err)
	}
}

func validateConfig(jsonBytes []byte) error {
	var conf struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonBytes, &conf); err != nil {
		return fmt.Errorf("error reading network config: %s", err)
	}
	if conf.Name == "" {
		return fmt.Errorf("missing network name")
	}
	return nil
}

func getCmdArgsFromEnv() (string, *skel.CmdArgs, error) {
	log.Printf("Going to read from stdin")
	stdinData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", nil, fmt.Errorf("error reading from stdin: %v", err)
	}

	cmdArgs := &skel.CmdArgs{
		ContainerID: os.Getenv("CNI_CONTAINERID"),
		Netns:       os.Getenv("CNI_NETNS"),
		IfName:      os.Getenv("CNI_IFNAME"),
		Args:        os.Getenv("CNI_ARGS"),
		Path:        os.Getenv("CNI_PATH"),
		StdinData:   stdinData,
	}

	cmd := os.Getenv("CNI_COMMAND")
	return cmd, cmdArgs, nil
}

func handleIfCniUpdate(update func(*skel.CmdArgs) error) (bool, error) {
	isupdate := true

	if os.Getenv("CNI_COMMAND") != cni.CmdUpdate {
		return false, nil
	}

	log.Printf("CNI UPDATE received.")

	_, cmdArgs, err := getCmdArgsFromEnv()
	if err != nil {
		log.Printf("Received error while retrieving cmds from environment: %+v", err)
		return isupdate, err
	}

	log.Printf("Retrieved command args for update +%v", cmdArgs)
	err = validateConfig(cmdArgs.StdinData)
	if err != nil {
		log.Printf("Failed to handle CNI UPDATE, err:%v.", err)
		return isupdate, err
	}

	err = update(cmdArgs)
	if err != nil {
		log.Printf("Failed to handle CNI UPDATE, err:%v.", err)
		return isupdate, err
	}

	return isupdate, nil
}

func printCNIError(msg string) {
	log.Errorf(msg)
	cniErr := &cniTypes.Error{
		Code: cniTypes.ErrTryAgainLater,
		Msg:  msg,
	}
	cniErr.Print()
}

func rootExecute() error {
	var (
		config common.PluginConfig
		tb     *telemetry.TelemetryBuffer
	)

	config.Version = version
	reportManager := &telemetry.ReportManager{
		HostNetAgentURL: hostNetAgentURL,
		ContentType:     telemetry.ContentType,
		Report: &telemetry.CNIReport{
			Context:          "AzureCNI",
			SystemDetails:    telemetry.SystemInfo{},
			InterfaceDetails: telemetry.InterfaceInfo{},
			BridgeDetails:    telemetry.BridgeInfo{},
			Version:          version,
		},
	}

	cniReport := reportManager.Report.(*telemetry.CNIReport)

	netPlugin, err := network.NewPlugin(
		name,
		&config,
		&nns.GrpcClient{},
		&network.Multitenancy{},
	)
	if err != nil {
		printCNIError(fmt.Sprintf("Failed to create network plugin, err:%v.\n", err))
		return errors.Wrap(err, "Create plugin error")
	}

	// Check CNI_COMMAND value
	cniCmd := os.Getenv(cni.Cmd)

	if cniCmd != cni.CmdVersion {
		log.Printf("CNI_COMMAND environment variable set to %s", cniCmd)

		cniReport.GetReport(pluginName, version, ipamQueryURL)

		var upTime time.Time
		upTime, err = platform.GetLastRebootTime()
		if err == nil {
			cniReport.VMUptime = upTime.Format("2006-01-02 15:04:05")
		}

		// CNI Acquires lock
		if err = netPlugin.Plugin.InitializeKeyValueStore(&config); err != nil {
			printCNIError(fmt.Sprintf("Failed to initialize key-value store of network plugin: %v", err))

			tb = telemetry.NewTelemetryBuffer()
			if tberr := tb.Connect(); tberr != nil {
				log.Errorf("Cannot connect to telemetry service:%v", tberr)
				return errors.Wrap(err, "lock acquire error")
			}

			reportPluginError(reportManager, tb, err)

			if errors.Is(err, store.ErrTimeoutLockingStore) {
				var cniMetric telemetry.AIMetric
				cniMetric.Metric = aitelemetry.Metric{
					Name:             telemetry.CNILockTimeoutStr,
					Value:            1.0,
					CustomDimensions: make(map[string]string),
				}
				sendErr := telemetry.SendCNIMetric(&cniMetric, tb)
				if sendErr != nil {
					log.Errorf("Couldn't send cnilocktimeout metric: %v", sendErr)
				}
			}

			tb.Close()
			return errors.Wrap(err, "lock acquire error")
		}

		defer func() {
			if errUninit := netPlugin.Plugin.UninitializeKeyValueStore(); errUninit != nil {
				log.Errorf("Failed to uninitialize key-value store of network plugin, err:%v.\n", errUninit)
			}

			if recover() != nil {
				os.Exit(1)
			}
		}()

		// Start telemetry process if not already started. This should be done inside lock, otherwise multiple process
		// end up creating/killing telemetry process results in undesired state.
		tb = telemetry.NewTelemetryBuffer()
		if err = tb.ConnectCNIToTelemetryService(telemetryNumRetries, telemetryWaitTimeInMilliseconds, netPlugin.Plugin); err != nil {
			log.Errorf("connection to telemetry service failed.")
		}
		defer tb.Close()
		netPlugin.SetCNIReport(cniReport, tb)

		t := time.Now()
		cniReport.Timestamp = t.Format("2006-01-02 15:04:05")

		if err = netPlugin.Start(&config); err != nil {
			printCNIError(fmt.Sprintf("Failed to start network plugin, err:%v.\n", err))
			reportPluginError(reportManager, tb, err)
			panic("network plugin start fatal error")
		}

		// used to dump state
		if cniCmd == cni.CmdGetEndpointsState {
			log.Printf("Retrieving state")
			var simpleState *api.AzureCNIState
			simpleState, err = netPlugin.GetAllEndpointState("azure")
			if err != nil {
				log.Errorf("Failed to get Azure CNI state, err:%v.\n", err)
				return errors.Wrap(err, "Get all endpoints error")
			}

			err = simpleState.PrintResult()
			if err != nil {
				log.Errorf("Failed to print state result to stdout with err %v\n", err)
			}

			return errors.Wrap(err, "Get cni state printresult error")
		}
	}

	handled, _ := handleIfCniUpdate(netPlugin.Update)
	if handled {
		log.Printf("CNI UPDATE finished.")
	} else if err = netPlugin.Execute(cni.PluginApi(netPlugin)); err != nil {
		log.Errorf("Failed to execute network plugin, err:%v.\n", err)
	}

	if cniCmd == cni.CmdVersion {
		return errors.Wrap(err, "Execute netplugin failure")
	}

	netPlugin.Stop()

	if err != nil {
		reportPluginError(reportManager, tb, err)
	}

	return errors.Wrap(err, "Execute netplugin failure")
}

// Main is the entry point for CNI network plugin.
func main() {
	// Initialize and parse command line arguments.
	common.ParseArgs(&args, printVersion)
	vers := common.GetArg(common.OptVersion).(bool)

	if vers {
		printVersion()
		os.Exit(0)
	}

	log.SetName(name)
	log.SetLevel(log.LevelInfo)
	if err := log.SetTargetLogDirectory(log.TargetLogfile, ""); err != nil {
		fmt.Printf("Failed to setup cni logging: %v\n", err)
		return
	}

	err := rootExecute()

	log.Close()
	if err != nil {
		os.Exit(1)
	}
}
