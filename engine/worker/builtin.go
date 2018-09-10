package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/golang/protobuf/ptypes/empty"

	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/grpcplugin/actionplugin"
	"github.com/ovh/cds/sdk/log"
	"github.com/ovh/cds/sdk/plugin"
)

var mapBuiltinActions = map[string]BuiltInActionFunc{}

func init() {
	mapBuiltinActions[sdk.ArtifactUpload] = runArtifactUpload
	mapBuiltinActions[sdk.ArtifactDownload] = runArtifactDownload
	mapBuiltinActions[sdk.ScriptAction] = runScriptAction
	mapBuiltinActions[sdk.JUnitAction] = runParseJunitTestResultAction
	mapBuiltinActions[sdk.GitCloneAction] = runGitClone
	mapBuiltinActions[sdk.GitTagAction] = runGitTag
	mapBuiltinActions[sdk.ReleaseAction] = runRelease
	mapBuiltinActions[sdk.CheckoutApplicationAction] = runCheckoutApplication
	mapBuiltinActions[sdk.DeployApplicationAction] = runDeployApplication
	mapBuiltinActions[sdk.CoverageAction] = runParseCoverageResultAction
}

// BuiltInAction defines builtin action signature
type BuiltInAction func(context.Context, *sdk.Action, int64, *[]sdk.Parameter, []sdk.Variable, LoggerFunc) sdk.Result

// BuiltInActionFunc returns the BuiltInAction given a worker
type BuiltInActionFunc func(*currentWorker) BuiltInAction

// LoggerFunc is the type for the logging function through BuiltInActions
type LoggerFunc func(format string)

func getLogger(w *currentWorker, buildID int64, stepOrder int) LoggerFunc {
	return func(s string) {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		w.sendLog(buildID, s, stepOrder, false)
	}
}

func (w *currentWorker) runBuiltin(ctx context.Context, a *sdk.Action, buildID int64, params *[]sdk.Parameter, secrets []sdk.Variable, stepOrder int) sdk.Result {
	log.Info("runBuiltin> Begin buildID:%d stepOrder:%d", buildID, stepOrder)
	defer func() {
		log.Info("runBuiltin> End buildID:%d stepOrder:%d", buildID, stepOrder)
	}()
	defer w.drainLogsAndCloseLogger(ctx)

	//Define a loggin function
	sendLog := getLogger(w, buildID, stepOrder)

	f, ok := mapBuiltinActions[a.Name]
	if !ok {
		res := sdk.Result{
			Status: sdk.StatusFail.String(),
			Reason: fmt.Sprintf("Unknown builtin step: %s\n", a.Name),
		}
		return res
	}

	return f(w)(ctx, a, buildID, params, secrets, sendLog)
}

func (w *currentWorker) runPlugin(ctx context.Context, a *sdk.Action, buildID int64, params *[]sdk.Parameter, stepOrder int, sendLog LoggerFunc) sdk.Result {
	log.Info("runPlugin> Begin buildID:%d stepOrder:%d", buildID, stepOrder)
	defer func() {
		log.Info("runPlugin> End buildID:%d stepOrder:%d", buildID, stepOrder)
	}()

	chanRes := make(chan sdk.Result, 1)

	go func(buildID int64, params []sdk.Parameter) {
		res := sdk.Result{Status: sdk.StatusFail.String()}

		//For the moment we consider that plugin name = action name = plugin binary file name
		pluginName := a.Name
		//The binary file has been downloaded during requirement check in /tmp
		pluginBinary := path.Join(w.basedir, a.Name)

		var tlsskipverify bool
		if os.Getenv("CDS_SKIP_VERIFY") != "" {
			tlsskipverify = true
		}

		env := []string{}
		//set up environment variables from pipeline build job parameters
		for _, p := range params {
			// avoid put private key in environment var as it's a binary value
			if (p.Type == sdk.KeyPGPParameter || p.Type == sdk.KeySSHParameter) && strings.HasSuffix(p.Name, ".priv") {
				continue
			}
			if p.Type == sdk.KeyParameter && !strings.HasSuffix(p.Name, ".pub") {
				continue
			}
			envName := strings.Replace(p.Name, ".", "_", -1)
			envName = strings.ToUpper(envName)
			env = append(env, fmt.Sprintf("%s=%s", envName, p.Value))
		}

		for _, p := range w.currentJob.buildVariables {
			envName := strings.Replace(p.Name, ".", "_", -1)
			envName = strings.ToUpper(envName)
			env = append(env, fmt.Sprintf("%s=%s", envName, p.Value))
		}

		//Create the rpc server
		pluginClient := plugin.NewClient(ctx, pluginName, pluginBinary, w.id, w.apiEndpoint, tlsskipverify, env...)
		defer pluginClient.Kill()

		//Get the plugin interface
		_plugin, err := pluginClient.Instance()
		if err != nil {
			result := sdk.Result{
				Status: sdk.StatusFail.String(),
				Reason: fmt.Sprintf("Unable to init plugin %s: %s\n", pluginName, err),
			}
			sendLog(result.Reason)
			chanRes <- result
		}

		sendLog(fmt.Sprintf("Starting plugin: %s version %s\n", _plugin.Name(), _plugin.Version()))
		log.Info("runPlugin> Starting plugin:%s version:%s stepOrder:%d", _plugin.Name(), _plugin.Version(), stepOrder)

		//Manage all parameters
		pluginSecrets := plugin.Secrets{
			Data: map[string]string{},
		}

		pluginArgs := plugin.Arguments{
			Data: map[string]string{},
		}
		for _, p := range a.Parameters {
			pluginArgs.Data[p.Name] = p.Value
		}
		for _, p := range params {
			pluginArgs.Data[p.Name] = p.Value
			if sdk.NeedPlaceholder(p.Type) {
				pluginSecrets.Data[p.Name] = p.Value
			}
		}
		for _, v := range w.currentJob.buildVariables {
			pluginArgs.Data[v.Name] = v.Value
		}

		//Call the Run function on the plugin interface
		id := w.currentJob.pbJob.PipelineBuildID
		if w.currentJob.wJob != nil {
			id = w.currentJob.wJob.WorkflowNodeRunID
		}

		pluginAction := plugin.Job{
			IDPipelineBuild:    id,
			IDPipelineJobBuild: buildID,
			OrderStep:          stepOrder,
			Args:               pluginArgs,
			Secrts:             pluginSecrets,
			HTTPPortWorker:     int(w.exportPort),
		}
		if w.currentJob.wJob != nil && w.currentJob.wJob.WorkflowNodeRunID > 0 {
			pluginAction.IDWorkflowNodeRun = w.currentJob.wJob.WorkflowNodeRunID
		}

		pluginResult := _plugin.Run(pluginAction)
		if pluginResult == plugin.Success {
			res.Status = sdk.StatusSuccess.String()
			log.Info("runPlugin> plugin success on stepOrder:%d", stepOrder)
		} else {
			res.Status = sdk.StatusFail.String()
			res.Reason = fmt.Sprintf("Plugin Failure: %v", pluginResult)
			log.Info("runPlugin> plugin failure on stepOrder:%d", stepOrder)
		}

		chanRes <- res
	}(buildID, *params)

	select {
	case <-ctx.Done():
		log.Error("CDS Worker execution canceled: %v", ctx.Err())
		w.sendLog(buildID, "CDS Worker execution canceled\n", stepOrder, false)
		return sdk.Result{
			Status: sdk.StatusFail.String(),
			Reason: "CDS Worker execution canceled",
		}
	case res := <-chanRes:
		return res
	}
}

func (w *currentWorker) runGRPCPlugin(ctx context.Context, a *sdk.Action, buildID int64, params *[]sdk.Parameter, stepOrder int, sendLog LoggerFunc) sdk.Result {
	log.Info("runPlugin> Begin buildID:%d stepOrder:%d", buildID, stepOrder)
	defer func() {
		log.Info("runPlugin> End buildID:%d stepOrder:%d", buildID, stepOrder)
	}()

	chanRes := make(chan sdk.Result, 1)

	go func(buildID int64, params []sdk.Parameter) {
		//For the moment we consider that plugin name = action name
		pluginName := a.Name
		pluginSocket, err := startGRPCPlugin(context.Background(), pluginName, w, nil, startGRPCPluginOptions{
			out: os.Stdout,
			err: os.Stderr,
		})
		if err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Unable to start grpc plugin... Aborting (%v)", err))
			return
		}

		c, err := actionplugin.Client(ctx, pluginSocket.Socket)
		if err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Unable to call grpc plugin... Aborting (%v)", err))
			return
		}
		qPort := actionplugin.WorkerHTTPPortQuery{Port: w.exportPort}
		if _, err := c.WorkerHTTPPort(ctx, &qPort); err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Unable to set worker http port for grpc plugin... Aborting (%v)", err))
			return
		}

		pluginSocket.Client = c

		m, err := c.Manifest(context.Background(), new(empty.Empty))
		if err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Unable to call grpc plugin manifest... Aborting (%v)", err))
			return
		}
		log.Info("plugin successfully initialized: %#v", m)

		pluginClient := pluginSocket.Client
		actionPluginClient, ok := pluginClient.(actionplugin.ActionPluginClient)
		if !ok {
			pluginFail(chanRes, sendLog, "Unable to retrieve plugin client... Aborting")
			return
		}

		logCtx, stopLogs := context.WithCancel(ctx)
		go enablePluginLogger(logCtx, sendLog, pluginSocket)
		defer stopLogs()

		manifest, err := actionPluginClient.Manifest(ctx, &empty.Empty{})
		if err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Unable to retrieve plugin manifest... Aborting (%v)", err))
			return
		}

		sendLog(fmt.Sprintf("# Plugin %s v%s is ready", manifest.Name, manifest.Version))
		query := actionplugin.ActionQuery{
			Options: sdk.ParametersMapMerge(sdk.ParametersToMap(params), sdk.ParametersToMap(a.Parameters)),
			JobID:   buildID,
		}

		result, err := actionPluginClient.Run(ctx, &query)
		if err != nil {
			pluginFail(chanRes, sendLog, fmt.Sprintf("Error running action: %v", err))
			return
		}

		chanRes <- sdk.Result{
			Status: result.GetStatus(),
			Reason: result.GetDetails(),
		}
	}(buildID, *params)

	select {
	case <-ctx.Done():
		log.Error("CDS Worker execution canceled: %v", ctx.Err())
		w.sendLog(buildID, "CDS Worker execution canceled\n", stepOrder, false)
		return sdk.Result{
			Status: sdk.StatusFail.String(),
			Reason: "CDS Worker execution canceled",
		}
	case res := <-chanRes:
		return res
	}
}

func pluginFail(chanRes chan<- sdk.Result, sendLog LoggerFunc, reason string) {
	res := sdk.Result{
		Reason: reason,
		Status: sdk.StatusFail.String(),
	}
	sendLog(res.Reason)
	chanRes <- res
}
