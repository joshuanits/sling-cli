package main

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/integrii/flaggy"
	"github.com/samber/lo"
	"gopkg.in/yaml.v2"

	"github.com/flarco/dbio"
	"github.com/flarco/dbio/connection"
	"github.com/flarco/g/net"
	core2 "github.com/slingdata-io/sling-cli/core"
	"github.com/slingdata-io/sling-cli/core/env"
	"github.com/slingdata-io/sling-cli/core/sling"

	"github.com/flarco/g"
	"github.com/jedib0t/go-pretty/table"
	"github.com/kardianos/osext"
	"github.com/spf13/cast"
)

var (
	masterServerURL = os.Getenv("SLING_MASTER_URL")
	apiKey          = os.Getenv("SLING_API_KEY")
	projectID       = os.Getenv("SLING_PROJECT")
	headers         = map[string]string{
		"Content-Type":     "application/json",
		"Sling-API-Key":    apiKey,
		"Sling-Project-ID": projectID,
	}
	updateAvailable = false
)

func init() {
	if masterServerURL == "" {
		masterServerURL = "https://api.slingdata.io"
	}
}

func processRun(c *g.CliSC) (ok bool, err error) {
	ok = true
	cfg := &sling.Config{}
	replicationCfgPath := ""
	cfgStr := ""
	showExamples := false
	// saveAsJob := false

	// determine if stdin data is piped
	// https://stackoverflow.com/a/26567513
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		cfg.Options.StdIn = true
	}

	for k, v := range c.Vals {
		switch k {
		case "replication":
			replicationCfgPath = cast.ToString(v)
		case "config":
			cfgStr = cast.ToString(v)
		case "src-conn":
			cfg.Source.Conn = cast.ToString(v)
		case "src-stream", "src-table", "src-sql", "src-file":
			cfg.Source.Stream = cast.ToString(v)
			if strings.Contains(cfg.Source.Stream, "://") {
				if _, ok := c.Vals["src-conn"]; !ok { // src-conn not specified
					cfg.Source.Conn = cfg.Source.Stream
				}
			}
		case "src-options":
			err = yaml.Unmarshal([]byte(cast.ToString(v)), &cfg.Source.Options)
			if err != nil {
				return ok, g.Error(err, "invalid source options -> %s", cast.ToString(v))
			}

		case "tgt-conn":
			cfg.Target.Conn = cast.ToString(v)

		case "primary-key":
			cfg.Source.PrimaryKey = strings.Split(cast.ToString(v), ",")

		case "update-key":
			cfg.Source.UpdateKey = cast.ToString(v)

		case "tgt-object", "tgt-table", "tgt-file":
			cfg.Target.Object = cast.ToString(v)
			if strings.Contains(cfg.Target.Object, "://") {
				if _, ok := c.Vals["tgt-conn"]; !ok { // tgt-conn not specified
					cfg.Target.Conn = cfg.Target.Object
				}
			}
		case "tgt-options":
			stdout := cfg.Options.StdOut
			err = yaml.Unmarshal([]byte(cast.ToString(v)), &cfg.Target.Options)
			if err != nil {
				return ok, g.Error(err, "invalid target options -> %s", cast.ToString(v))
			}
			if stdout {
				cfg.Options.StdOut = stdout
			}
		case "stdout":
			cfg.Options.StdOut = cast.ToBool(v)
		case "mode":
			cfg.Mode = sling.Mode(cast.ToString(v))
		case "debug":
			cfg.Options.Debug = cast.ToBool(v)
		case "examples":
			showExamples = cast.ToBool(v)
		}
	}

	if showExamples {
		println(examples)
		return ok, nil
	}

	os.Setenv("SLING_CLI", "TRUE")

	// check for update, and print note
	go isUpdateAvailable()
	defer printUpdateAvailable()

	if replicationCfgPath != "" {
		err = runReplication(replicationCfgPath)
		if err != nil {
			return ok, g.Error(err, "failure running replication (see docs @ https://docs.slingdata.io/sling-cli)")
		}
		return ok, nil

	}

	if cfgStr != "" {
		err = cfg.Unmarshal(cfgStr)
		if err != nil {
			return ok, g.Error(err, "could not parse task configuration (see docs @ https://docs.slingdata.io/sling-cli)")
		}
	}

	// run task
	err = runTask(cfg)
	if err != nil {
		return ok, g.Error(err, "failure running task (see docs @ https://docs.slingdata.io/sling-cli)")
	}

	return ok, nil
}

func runTask(cfg *sling.Config) (err error) {
	var task *sling.TaskExecution

	// track usage
	defer func() {
		if task != nil {
			inBytes, outBytes := task.GetBytes()
			telemetryMap["task_type"] = task.Type
			telemetryMap["task_mode"] = task.Config.Mode
			telemetryMap["task_status"] = task.Status
			telemetryMap["task_source_type"] = task.Config.SrcConn.Type
			telemetryMap["task_target_type"] = task.Config.TgtConn.Type
			telemetryMap["task_start_time"] = task.StartTime
			telemetryMap["task_end_time"] = task.EndTime
			telemetryMap["task_rows_count"] = task.GetCount()
			telemetryMap["task_rows_in_bytes"] = inBytes
			telemetryMap["task_rows_out_bytes"] = outBytes
			if cfg.Options.StdOut {
				telemetryMap["task_target_type"] = "stdout"
			}
		}

		if err != nil {
			telemetryMap["error"] = getErrString(err)
		}

		// telemetry
		Track("run")
	}()

	err = cfg.Prepare()
	if err != nil {
		err = g.Error(err, "could not set task configuration")
		return
	}

	task = sling.NewTask(0, cfg)
	if task.Err != nil {
		err = g.Error(task.Err)
		return
	}

	// set context
	task.Context = &ctx

	// run task
	err = task.Execute()
	if err != nil {
		return g.Error(err)
	}

	return nil
}

func runReplication(cfgPath string) (err error) {
	replication, err := sling.LoadReplicationConfig(cfgPath)
	if err != nil {
		return g.Error(err, "Error parsing replication config")
	}

	g.Info("Sling Replication [%d streams] | %s -> %s", len(replication.Streams), replication.Source, replication.Target)

	eG := g.ErrorGroup{}
	for i, name := range sort.StringSlice(lo.Keys(replication.Streams)) {
		if interrupted {
			break
		}

		stream := replication.Streams[name]
		if stream == nil {
			stream = &sling.ReplicationStreamConfig{}
		}
		sling.SetStreamDefaults(stream, replication)

		if stream.Object == "" {
			return g.Error("need to specify `object`. Please see https://docs.slingdata.io/sling-cli for help.")
		}

		cfg := sling.Config{
			Source: sling.Source{
				Conn:       replication.Source,
				Stream:     name,
				Columns:    stream.Columns,
				PrimaryKey: stream.PrimaryKey,
				UpdateKey:  stream.UpdateKey,
			},
			Target: sling.Target{
				Conn:   replication.Target,
				Object: stream.Object,
			},
			Mode: stream.Mode,
		}

		// so that the next stream does not retain previous pointer values
		g.Unmarshal(g.Marshal(stream.SourceOptions), cfg.Source.Options)
		g.Unmarshal(g.Marshal(stream.TargetOptions), cfg.Target.Options)

		if stream.SQL != "" {
			cfg.Source.Stream = stream.SQL
		}

		println()
		g.Info("[%d / %d] running stream `%s`", i+1, len(replication.Streams), name)

		err = runTask(&cfg)
		if err != nil {
			err = g.Error(err, "error for stream `%s`", name)
			g.LogError(err)
			eG.Capture(err)
		}
		telemetryMap = g.M("begin_time", time.Now().UnixMicro()) // reset map
	}

	return eG.Err()
}

func processConns(c *g.CliSC) (bool, error) {
	ok := true
	switch c.UsedSC() {
	case "unset":
		name := strings.ToUpper(cast.ToString(c.Vals["name"]))
		if name == "" {
			flaggy.ShowHelp("")
			return ok, nil
		}

		ef := env.LoadSlingEnvFile()
		_, ok := ef.Connections[name]
		if !ok {
			return true, g.Error("did not find connection `%s`", name)
		}

		delete(ef.Connections, name)
		err := env.WriteSlingEnvFile(ef)
		if err != nil {
			return ok, g.Error(err, "could not write env file")
		}
		g.Info("connection `%s` has been removed from %s", name, env.HomeDirEnvFile)
	case "set":
		if len(c.Vals) == 0 {
			flaggy.ShowHelp("")
			return ok, nil
		}

		url := ""
		kvArr := []string{cast.ToString(c.Vals["value properties..."])}
		kvMap := map[string]interface{}{}
		for k, v := range g.KVArrToMap(append(kvArr, flaggy.TrailingArguments...)...) {
			k = strings.ToLower(k)
			if k == "url" {
				url = v
			}
			kvMap[k] = v
		}
		name := strings.ToUpper(cast.ToString(c.Vals["name"]))

		// parse url
		if url != "" {
			conn, err := connection.NewConnectionFromURL(name, url)
			if err != nil {
				return ok, g.Error(err, "could not parse url")
			}
			if _, ok := kvMap["type"]; !ok {
				kvMap["type"] = conn.Type.String()
			}
		}

		t, found := kvMap["type"]
		if _, typeOK := dbio.ValidateType(cast.ToString(t)); found && !typeOK {
			return ok, g.Error("invalid type (%s). See https://docs.slingdata.io/sling-cli/environment#set-connections for help.", cast.ToString(t))
		} else if !found {
			return ok, g.Error("need to specify valid `type` key or provide `url`. See https://docs.slingdata.io/sling-cli/environment#set-connections for help.")
		}

		ef := env.LoadSlingEnvFile()
		ef.Connections[name] = kvMap
		err := env.WriteSlingEnvFile(ef)
		if err != nil {
			return ok, g.Error(err, "could not write env file")
		}
		g.Info("connection `%s` has been set in %s. Please test with `sling conns test %s`", name, env.HomeDirEnvFile, name)

	case "list", "show":
		conns := connection.GetLocalConns()
		T := table.NewWriter()
		T.AppendHeader(table.Row{"Conn Name", "Conn Type", "Source"})
		for _, conn := range conns {
			T.AppendRow(table.Row{conn.Name, conn.Description, conn.Source})
		}
		println(T.Render())
	case "discover", "test":
		name := cast.ToString(c.Vals["name"])
		filter := cast.ToString(c.Vals["filter"])
		if len(c.Vals) == 0 || name == "" {
			flaggy.ShowHelp("")
			return ok, nil
		}

		discover := c.UsedSC() == "discover"
		schema := cast.ToString(c.Vals["schema"])
		folder := cast.ToString(c.Vals["folder"])

		conns := map[string]connection.ConnEntry{}
		for _, conn := range connection.GetLocalConns() {
			conns[strings.ToLower(conn.Name)] = conn
		}

		conn, ok1 := conns[strings.ToLower(name)]
		if !ok1 || name == "" {
			g.Warn("Invalid Connection name: %s", name)
			return ok, nil
		}

		telemetryMap["conn_type"] = conn.Connection.Type.String()

		streamNames := []string{}
		switch {

		case conn.Connection.Type.IsDb():
			dbConn, err := conn.Connection.AsDatabase()
			if err != nil {
				return ok, g.Error(err, "could not initiate %s", name)
			}
			err = dbConn.Connect()
			if err != nil {
				return ok, g.Error(err, "could not connect to %s (see docs @ https://docs.slingdata.io/sling-cli)", name)
			}
			if discover {
				schemata, err := dbConn.GetSchemata(schema, "")
				if err != nil {
					return ok, g.Error(err, "could not discover %s", name)
				}
				for _, table := range schemata.Tables() {
					streamNames = append(streamNames, table.FullName())
				}
			}

		case conn.Connection.Type.IsFile():
			fileClient, err := conn.Connection.AsFile()
			if err != nil {
				return ok, g.Error(err, "could not initiate %s", name)
			}
			err = fileClient.Init(context.Background())
			if err != nil {
				return ok, g.Error(err, "could not connect to %s (see docs @ https://docs.slingdata.io/sling-cli)", name)
			}

			url := conn.Connection.URL()
			if folder != "" {
				if !strings.HasPrefix(folder, string(fileClient.FsType())+"://") {
					return ok, g.Error("need to use proper URL for folder path. Example -> %s/my-folder", url)
				}
				url = folder
			}

			streamNames, err = fileClient.List(url)
			if err != nil {
				return ok, g.Error(err, "could not connect to %s (see docs @ https://docs.slingdata.io/sling-cli)", name)
			}

		case conn.Connection.Type.IsAirbyte():
			client, err := conn.Connection.AsAirbyte()
			if err != nil {
				return ok, g.Error(err, "could not initiate %s", name)
			}
			err = client.Init(!discover)
			if err != nil {
				return ok, g.Error(err, "could not connect to %s", name)
			}
			if discover {
				streams, err := client.Discover()
				if err != nil {
					return ok, g.Error(err, "could not discover %s", name)
				}
				streamNames = streams.Names()
			}

		default:
			return ok, g.Error("Unhandled connection type: %s (see docs @ https://docs.slingdata.io/sling-cli)", conn.Connection.Type)
		}

		if discover {
			// sort alphabetically
			sort.Slice(streamNames, func(i, j int) bool {
				return streamNames[i] < streamNames[j]
			})

			g.Info("Found %d streams:", len(streamNames))
			filters := strings.Split(filter, ",")
			for _, sn := range streamNames {
				if filter == "" || g.IsMatched(filters, sn) {
					println(g.F(" - %s", sn))
				}
			}
			if len(streamNames) > 0 && conn.Connection.Type.IsFile() &&
				folder == "" {
				println()
				g.Warn("Those are non-recursive folder or file names (at the root level). Please use --folder flag to list sub-folders")
			}
		} else {
			g.Info("success!") // successfully connected
		}

	case "":
		flaggy.ShowHelp("")
	}
	return ok, nil
}

func slingUiServer(c *g.CliSC) (ok bool, err error) {
	Track("slingUiServer")
	ok = true
	return
}

func checkLatestVersion() (string, error) {
	url := ""
	if runtime.GOOS == "linux" {
		url = "https://files.ocral.org/slingdata.io/dist/version-linux"
	} else if runtime.GOOS == "darwin" {
		url = "https://files.ocral.org/slingdata.io/dist/version-mac"
	} else if runtime.GOOS == "windows" {
		url = "https://files.ocral.org/slingdata.io/dist/version-win"
	} else {
		return "", g.Error("OS Unsupported: %s", runtime.GOOS)
	}

	_, respBytes, err := net.ClientDo("GET", url, nil, map[string]string{}, 5)
	newVersion := strings.TrimSpace(string(respBytes))
	if err != nil {
		return "", g.Error(err, "Unable to check for latest version")
	}
	return newVersion, nil
}

func isUpdateAvailable() bool {
	newVersion, err := checkLatestVersion()
	if err != nil {
		return false
	}
	updateAvailable = newVersion != core2.Version
	return updateAvailable
}

func printUpdateAvailable() {
	if updateAvailable {
		g.Warn("FYI, a new sling version is available (please run `sling update`)")
	}
}

func updateCLI(c *g.CliSC) (ok bool, err error) {
	// Print Progress: https://gist.github.com/albulescu/e61979cc852e4ee8f49c

	ok = true
	telemetryMap["downloaded"] = false

	// get latest version number
	newVersion, err := checkLatestVersion()
	if err != nil {
		return ok, g.Error(err)
	} else if newVersion == core2.Version {
		g.Info("Already up-to-date!")
		return
	}

	telemetryMap["new_version"] = newVersion
	url := ""
	if runtime.GOOS == "linux" {
		url = "https://files.ocral.org/slingdata.io/dist/sling-linux"
	} else if runtime.GOOS == "darwin" {
		url = "https://files.ocral.org/slingdata.io/dist/sling-mac"
	} else if runtime.GOOS == "windows" {
		url = "https://files.ocral.org/slingdata.io/dist/sling-win.exe"
	} else {
		return ok, g.Error("OS Unsupported: %s", runtime.GOOS)
	}

	execFileName, err := osext.Executable()
	if err != nil {
		return ok, g.Error(err, "Unable to determine executable path")
	} else if strings.Contains(execFileName, "homebrew") {
		g.Warn("Sling was installed with brew, please run `brew upgrade slingdata-io/sling/sling`")
		return ok, nil
	} else if strings.Contains(execFileName, "scoop") {
		g.Warn("Sling was installed with scoop, please run `scoop update sling`")
		return ok, nil
	}

	fileStat, _ := os.Stat(execFileName)
	fileMode := fileStat.Mode()

	filePath := execFileName + ".new"

	g.Info("Downloading latest version (%s)", newVersion)
	err = net.DownloadFile(url, filePath)
	if err != nil {
		println("Unable to download update!")
		return ok, g.Error(strings.ReplaceAll(err.Error(), url, ""))
	}

	telemetryMap["downloaded"] = true

	err = os.Chmod(filePath, fileMode)
	if err != nil {
		println("Unable to make new binary executable.")
		return ok, err
	}

	err = os.Rename(execFileName, execFileName+".old")
	if err != nil {
		println("Unable to rename current binary executable. Try with sudo or admin?")
		return ok, err
	}

	err = os.Rename(filePath, execFileName)
	if err != nil {
		println("Unable to rename current binary executable. Try with sudo or admin?")
		return ok, err
	}

	os.Remove(execFileName + ".old")

	g.Info("Updated to " + strings.TrimSpace(string(newVersion)))

	return ok, nil
}
