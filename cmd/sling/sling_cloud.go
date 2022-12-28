package main

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/flarco/dbio/iop"
	"github.com/flarco/g"
	"github.com/flarco/g/net"
	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core/sling"
	"github.com/spf13/cast"
	"gopkg.in/yaml.v2"
)

type RouteName string

const (
	RouteAPIProjects     RouteName = "/api/v1/projects"
	RouteAPIReplications RouteName = "/api/v1/replications"
	RouteAPIExport       RouteName = "/api/v1/export"
	RouteAPIImport       RouteName = "/api/v1/import"
)

// ClientPost sends a POST request
func ClientPost(serverURL string, route RouteName, m map[string]interface{}, headers map[string]string) (respStr string, err error) {
	URL := serverURL + string(route)
	jsonBytes, err := json.Marshal(m)
	if err != nil {
		err = g.Error(err)
		return
	}
	_, respBytes, err := net.ClientDo(
		"POST",
		URL,
		bytes.NewBuffer(jsonBytes),
		headers,
	)
	respStr = string(respBytes)

	if err != nil {
		err = g.Error(err, "error sending POST to url")
		return
	}

	return
}

// ClientPut sends a PUT request
func ClientPut(serverURL string, route RouteName, m map[string]interface{}, headers map[string]string) (respStr string, err error) {
	URL := serverURL + string(route)
	jsonBytes, err := json.Marshal(m)
	if err != nil {
		err = g.Error(err)
		return
	}
	_, respBytes, err := net.ClientDo(
		"PUT",
		URL,
		bytes.NewBuffer(jsonBytes),
		headers,
	)
	respStr = string(respBytes)

	if err != nil {
		err = g.Error(err, "error sending PUT to url")
		return
	}

	return
}

// ListResponse is a response for a listing GET request
type ListResponse struct {
	Columns    []string                 `json:"columns"`
	Data       []map[string]interface{} `json:"data"`
	NextOffset int                      `json:"next_offset"`
}

// ClientGet sends a GET request
func ClientGet(serverURL string, route RouteName, m map[string]interface{}, headers map[string]string) (respStr string, err error) {
	vals := url.Values{}
	for k, v := range m {
		switch v.(type) {
		case map[string]interface{}:
			v = string(g.MarshalMap(v.(map[string]interface{})))
		}
		val := cast.ToString(v)
		if val == "" {
			continue
		}
		vals.Set(k, val)
	}
	URL := serverURL + string(route) + "?" + vals.Encode()

	_, respBytes, err := net.ClientDo(
		"GET",
		URL,
		nil,
		headers,
	)
	respStr = string(respBytes)
	if err != nil {
		err = g.Error(err, "error sending GET to url")
		return
	}

	return respStr, nil
}

func put(route RouteName, m map[string]interface{}) error {
	g.Debug("", m)
	_, err := ClientPut(
		masterServerURL, route, m, headers,
	)
	if err != nil {
		err = g.Error(err, "error in getting response")
	} else {
		println("OK")
	}
	return err
}

func post(route RouteName, m map[string]interface{}) error {
	g.Debug("", m)
	_, err := ClientPost(
		masterServerURL, route, m, headers,
	)
	if err != nil {
		err = g.Error(err, "error in getting response")
	} else {
		println("OK")
	}
	return err
}

func listReplications() (data iop.Dataset, err error) {

	respStr, err := ClientGet(masterServerURL, RouteAPIReplications, g.M(), headers)
	if err != nil {
		return data, g.Error(err, "Could not list replications")
	}

	resp := ListResponse{}
	err = g.Unmarshal(respStr, &resp)
	if err != nil {
		return data, g.Error(err, "Could not unmarshal replication list")
	}

	data.Columns = iop.Columns{
		{Name: "id", Type: iop.IntegerType},
		{Name: "source_name", Type: iop.StringType},
		{Name: "target_name", Type: iop.StringType},
		{Name: "active", Type: iop.BoolType},
		{Name: "project_id", Type: iop.StringType},
	}
	for _, rec := range resp.Data {
		row := lo.Map(data.Columns.Names(), func(c string, i int) any {
			if c == "active" && rec[c] == nil {
				return false
			}
			return rec[c]
		})
		data.Append(row)
	}

	data.Sort(4, 0) // sort by project_id, then ID

	return
}

func processCloud(c *g.CliSC) (bool, error) {
	ok := true

	if apiKey == "" {
		g.Warn("Please provide an API key in environment variable SLING_API_KEY. If you don't have one, get one at https://app.slingdata.io")
		return ok, nil
	}

	switch c.UsedSC() {
	case "deploy":
		path := cast.ToString(c.Vals["path"])
		stat, err := os.Stat(path)
		if os.IsNotExist(err) {
			return ok, g.Error(err, "Path does not exists: %s", path)
		}

		var files []g.FileItem
		if stat.IsDir() {
			files, err = g.ListDirRecursive(path)
			if err != nil {
				return ok, g.Error(err, "Could not list files in directory: %s", path)
			}
		} else {
			files = []g.FileItem{
				{
					Name:       stat.Name(),
					FullPath:   path,
					ParentPath: filepath.Dir(path),
					IsDir:      stat.IsDir(),
					ModTs:      stat.ModTime().Unix(),
					Size:       stat.Size(),
				},
			}
		}

		URL := masterServerURL + string(RouteAPIImport)
		for _, file := range files {
			if strings.HasSuffix(file.Name, ".yaml") || strings.HasSuffix(file.Name, ".yml") {
				replication, err := sling.LoadReplicationConfig(path)
				if err != nil {
					return ok, g.Error(err, "Could not load replication config: %s", path)
				}

				payload, _ := yaml.Marshal(replication)
				_, respB, err := net.ClientDo("POST", URL, bytes.NewBuffer(payload), headers)
				if err != nil {
					return ok, g.Error(err, "Could not import replication: %s", path)
				}
				respM := g.M()
				g.Unmarshal(string(respB), &respM)
				g.Info("successfully deployed replication #%d (%s)", cast.ToInt(respM["id"]), path)
			}
		}

	case "export":
		m := g.M("level", "replication", "id", c.Vals["id"])
		respStr, err := ClientPost(masterServerURL, RouteAPIExport, m, headers)
		if err != nil {
			return ok, g.Error(err, "Could not list replications")
		}

		respM := g.M()
		err = g.Unmarshal(respStr, &respM)
		if err != nil {
			return ok, g.Error(err, "Could not unmarshal export payload")
		}

		path := cast.ToString(c.Vals["path"])
		yamlPayload := cast.ToString(respM["output"])
		err = ioutil.WriteFile(path, []byte(yamlPayload), 0755)
		if err != nil {
			return ok, g.Error(err, "Could not write to: %s", path)
		}

		g.Info("Wrote to %s", path)
	case "list":
		data, err := listReplications()
		if err != nil {
			return ok, g.Error(err, "Could not list replications")
		}

		data.Print(100)
	case "trigger":
	default:
		return false, nil
	}
	return ok, nil
}
