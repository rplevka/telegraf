//go:generate ../../../tools/readme_config_includer/generator
package cassandra

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
)

// DO NOT REMOVE THE NEXT TWO LINES! This is required to embed the sampleConfig data.
//
//go:embed sample.conf
var sampleConfig string

type JolokiaClient interface {
	MakeRequest(req *http.Request) (*http.Response, error)
}

type JolokiaClientImpl struct {
	client *http.Client
}

func (c JolokiaClientImpl) MakeRequest(req *http.Request) (*http.Response, error) {
	return c.client.Do(req)
}

type Cassandra struct {
	jClient JolokiaClient
	Context string          `toml:"context"`
	Servers []string        `toml:"servers"`
	Metrics []string        `toml:"metrics"`
	Log     telegraf.Logger `toml:"-"`
}

type javaMetric struct {
	host   string
	metric string
	acc    telegraf.Accumulator
}

type cassandraMetric struct {
	host   string
	metric string
	acc    telegraf.Accumulator
}

type jmxMetric interface {
	addTagsFields(out map[string]interface{})
}

func newJavaMetric(acc telegraf.Accumulator, host string, metric string) *javaMetric {
	return &javaMetric{host: host, metric: metric, acc: acc}
}

func newCassandraMetric(acc telegraf.Accumulator, host string, metric string) *cassandraMetric {
	return &cassandraMetric{host: host, metric: metric, acc: acc}
}

func addValuesAsFields(values map[string]interface{}, fields map[string]interface{},
	mname string) {
	for k, v := range values {
		switch v.(type) {
		case int64, float64, string, bool:
			fields[mname+"_"+k] = v
		}
	}
}

func parseJmxMetricRequest(mbean string) map[string]string {
	tokens := make(map[string]string)
	classAndPairs := strings.Split(mbean, ":")
	switch classAndPairs[0] {
	case "org.apache.cassandra.metrics":
		tokens["class"] = "cassandra"
	case "java.lang":
		tokens["class"] = "java"
	default:
		return tokens
	}

	pairs := strings.Split(classAndPairs[1], ",")
	for _, pair := range pairs {
		p := strings.Split(pair, "=")
		tokens[p[0]] = p[1]
	}
	return tokens
}

func addTokensToTags(tokens map[string]string, tags map[string]string) {
	for k, v := range tokens {
		if k == "name" {
			tags["mname"] = v // name seems to a reserved word in influxdb
		} else if k == "class" || k == "type" {
			continue // class and type are used in the metric name
		} else {
			tags[k] = v
		}
	}
}

func (j javaMetric) addTagsFields(out map[string]interface{}) {
	tags := make(map[string]string)
	fields := make(map[string]interface{})

	a := out["request"].(map[string]interface{})
	attribute := a["attribute"].(string)
	mbean := a["mbean"].(string)

	tokens := parseJmxMetricRequest(mbean)
	addTokensToTags(tokens, tags)
	tags["cassandra_host"] = j.host

	if _, ok := tags["mname"]; !ok {
		//Queries for a single value will not return a "name" tag in the response.
		tags["mname"] = attribute
	}

	if values, ok := out["value"]; ok {
		switch t := values.(type) {
		case map[string]interface{}:
			addValuesAsFields(values.(map[string]interface{}), fields, attribute)
		case int64, float64, string, bool:
			fields[attribute] = t
		}
		j.acc.AddFields(tokens["class"]+tokens["type"], fields, tags)
	} else {
		j.acc.AddError(fmt.Errorf("missing key 'value' in '%s' output response: %v", j.metric, out))
	}
}

func addCassandraMetric(mbean string, c cassandraMetric, values map[string]interface{}) {
	tags := make(map[string]string)
	fields := make(map[string]interface{})
	tokens := parseJmxMetricRequest(mbean)
	addTokensToTags(tokens, tags)
	tags["cassandra_host"] = c.host
	addValuesAsFields(values, fields, tags["mname"])
	c.acc.AddFields(tokens["class"]+tokens["type"], fields, tags)
}

func (c cassandraMetric) addTagsFields(out map[string]interface{}) {
	r := out["request"]

	tokens := parseJmxMetricRequest(r.(map[string]interface{})["mbean"].(string))
	// Requests with wildcards for keyspace or table names will return nested
	// maps in the json response
	if (tokens["type"] == "Table" || tokens["type"] == "ColumnFamily") && (tokens["keyspace"] == "*" ||
		tokens["scope"] == "*") {
		valuesMap, ok := out["value"]
		if !ok {
			c.acc.AddError(fmt.Errorf("missing key 'value' in '%s' output response: %v", c.metric, out))
			return
		}
		for k, v := range valuesMap.(map[string]interface{}) {
			addCassandraMetric(k, c, v.(map[string]interface{}))
		}
	} else {
		values, ok := out["value"]
		if !ok {
			c.acc.AddError(fmt.Errorf("missing key 'value' in '%s' output response: %v", c.metric, out))
			return
		}
		addCassandraMetric(r.(map[string]interface{})["mbean"].(string), c, values.(map[string]interface{}))
	}
}

func (c *Cassandra) getAttr(requestURL *url.URL) (map[string]interface{}, error) {
	// Create + send request
	req, err := http.NewRequest("GET", requestURL.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.jClient.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Process response
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("response from url \"%s\" has status code %d (%s), expected %d (%s)",
			requestURL,
			resp.StatusCode,
			http.StatusText(resp.StatusCode),
			http.StatusOK,
			http.StatusText(http.StatusOK))
		return nil, err
	}

	// read body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Unmarshal json
	var jsonOut map[string]interface{}
	if err = json.Unmarshal(body, &jsonOut); err != nil {
		return nil, errors.New("error decoding JSON response")
	}

	return jsonOut, nil
}

func parseServerTokens(server string) map[string]string {
	serverTokens := make(map[string]string)

	hostAndUser := strings.Split(server, "@")
	hostPort := ""
	userPasswd := ""
	if len(hostAndUser) == 2 {
		hostPort = hostAndUser[1]
		userPasswd = hostAndUser[0]
	} else {
		hostPort = hostAndUser[0]
	}
	hostTokens := strings.Split(hostPort, ":")
	serverTokens["host"] = hostTokens[0]
	serverTokens["port"] = hostTokens[1]

	if userPasswd != "" {
		userTokens := strings.Split(userPasswd, ":")
		serverTokens["user"] = userTokens[0]
		serverTokens["passwd"] = userTokens[1]
	}
	return serverTokens
}

func (*Cassandra) SampleConfig() string {
	return sampleConfig
}

func (c *Cassandra) Start(_ telegraf.Accumulator) error {
	c.Log.Warn("DEPRECATED: The cassandra plugin has been deprecated. " +
		"Please use the jolokia2 plugin instead. " +
		"https://github.com/influxdata/telegraf/tree/master/plugins/inputs/jolokia2")
	return nil
}

func (c *Cassandra) Stop() {
}

func (c *Cassandra) Gather(acc telegraf.Accumulator) error {
	context := c.Context
	servers := c.Servers
	metrics := c.Metrics

	for _, server := range servers {
		for _, metric := range metrics {
			serverTokens := parseServerTokens(server)

			var m jmxMetric
			if strings.HasPrefix(metric, "/java.lang:") {
				m = newJavaMetric(acc, serverTokens["host"], metric)
			} else if strings.HasPrefix(metric,
				"/org.apache.cassandra.metrics:") {
				m = newCassandraMetric(acc, serverTokens["host"], metric)
			} else {
				// unsupported metric type
				acc.AddError(fmt.Errorf("unsupported Cassandra metric [%s], skipping", metric))
				continue
			}

			// Prepare URL
			requestURL, err := url.Parse("http://" + serverTokens["host"] + ":" +
				serverTokens["port"] + context + metric)
			if err != nil {
				acc.AddError(err)
				continue
			}
			if serverTokens["user"] != "" && serverTokens["passwd"] != "" {
				requestURL.User = url.UserPassword(serverTokens["user"],
					serverTokens["passwd"])
			}

			out, err := c.getAttr(requestURL)
			if err != nil {
				acc.AddError(err)
				continue
			}
			if out["status"] != 200.0 {
				acc.AddError(fmt.Errorf("provided URL returned with status %v - %s", out["status"], requestURL))
				continue
			}
			m.addTagsFields(out)
		}
	}
	return nil
}

func init() {
	inputs.Add("cassandra", func() telegraf.Input {
		return &Cassandra{jClient: &JolokiaClientImpl{client: &http.Client{}}}
	})
}
