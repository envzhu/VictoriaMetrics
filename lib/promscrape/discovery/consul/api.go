package consul

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
	"github.com/VictoriaMetrics/fasthttp"
)

var waitTime = flag.Duration("promscrape.consul.waitTime", 0, "Wait time used by Consul service discovery. Default value is used if not set")

// apiConfig contains config for API server.
type apiConfig struct {
	tagSeparator  string
	consulWatcher *consulWatcher
}

func (ac *apiConfig) mustStop() {
	ac.consulWatcher.mustStop()
}

var configMap = discoveryutils.NewConfigMap()

func getAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	v, err := configMap.Get(sdc, func() (interface{}, error) { return newAPIConfig(sdc, baseDir) })
	if err != nil {
		return nil, err
	}
	return v.(*apiConfig), nil
}

func newAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	token, err := getToken(sdc.Token)
	if err != nil {
		return nil, err
	}
	var ba *promauth.BasicAuthConfig
	if len(sdc.Username) > 0 {
		ba = &promauth.BasicAuthConfig{
			Username: sdc.Username,
			Password: sdc.Password,
		}
		token = ""
	}
	ac, err := promauth.NewConfig(baseDir, nil, ba, token, "", nil, sdc.TLSConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot parse auth config: %w", err)
	}
	apiServer := sdc.Server
	if apiServer == "" {
		apiServer = "localhost:8500"
	}
	if !strings.Contains(apiServer, "://") {
		scheme := sdc.Scheme
		if scheme == "" {
			scheme = "http"
		}
		apiServer = scheme + "://" + apiServer
	}
	proxyAC, err := sdc.ProxyClientConfig.NewConfig(baseDir)
	if err != nil {
		return nil, fmt.Errorf("cannot parse proxy auth config: %w", err)
	}
	client, err := discoveryutils.NewClient(apiServer, ac, sdc.ProxyURL, proxyAC)
	if err != nil {
		return nil, fmt.Errorf("cannot create HTTP client for %q: %w", apiServer, err)
	}
	tagSeparator := ","
	if sdc.TagSeparator != nil {
		tagSeparator = *sdc.TagSeparator
	}
	dc, err := getDatacenter(client, sdc.Datacenter)
	if err != nil {
		return nil, err
	}

	cw := newConsulWatcher(client, sdc, dc)
	cfg := &apiConfig{
		tagSeparator:  tagSeparator,
		consulWatcher: cw,
	}
	return cfg, nil
}

func getToken(token *string) (string, error) {
	if token != nil {
		return *token, nil
	}
	if tokenFile := os.Getenv("CONSUL_HTTP_TOKEN_FILE"); tokenFile != "" {
		data, err := ioutil.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("cannot read consul token file %q; probably, `token` arg is missing in `consul_sd_config`? error: %w", tokenFile, err)
		}
		return string(data), nil
	}
	t := os.Getenv("CONSUL_HTTP_TOKEN")
	// Allow empty token - it shouls work if authorization is disabled in Consul
	return t, nil
}

func getDatacenter(client *discoveryutils.Client, dc string) (string, error) {
	if dc != "" {
		return dc, nil
	}
	// See https://www.consul.io/api/agent.html#read-configuration
	data, err := client.GetAPIResponse("/v1/agent/self")
	if err != nil {
		return "", fmt.Errorf("cannot query consul agent info: %w", err)
	}
	a, err := parseAgent(data)
	if err != nil {
		return "", err
	}
	return a.Config.Datacenter, nil
}

// maxWaitTime is duration for consul blocking request.
func maxWaitTime() time.Duration {
	d := discoveryutils.BlockingClientReadTimeout
	// Consul adds random delay up to wait/16, so reduce the timeout in order to keep it below BlockingClientReadTimeout.
	// See https://www.consul.io/api-docs/features/blocking
	d -= d / 8
	// The timeout cannot exceed 10 minuntes. See https://www.consul.io/api-docs/features/blocking
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	if *waitTime > time.Second && *waitTime < d {
		d = *waitTime
	}
	return d
}

// getBlockingAPIResponse perfoms blocking request to Consul via client and returns response.
//
// See https://www.consul.io/api-docs/features/blocking .
func getBlockingAPIResponse(client *discoveryutils.Client, path string, index int64) ([]byte, int64, error) {
	path += "&index=" + strconv.FormatInt(index, 10)
	path += "&wait=" + fmt.Sprintf("%ds", int(maxWaitTime().Seconds()))
	getMeta := func(resp *fasthttp.Response) {
		ind := resp.Header.Peek("X-Consul-Index")
		if len(ind) == 0 {
			logger.Errorf("cannot find X-Consul-Index header in response from %q", path)
			return
		}
		newIndex, err := strconv.ParseInt(string(ind), 10, 64)
		if err != nil {
			logger.Errorf("cannot parse X-Consul-Index header value in response from %q: %s", path, err)
			return
		}
		// Properly handle the returned newIndex according to https://www.consul.io/api-docs/features/blocking#implementation-details
		if newIndex < 1 {
			index = 1
			return
		}
		if index > newIndex {
			index = 0
			return
		}
		index = newIndex
	}
	data, err := client.GetBlockingAPIResponse(path, getMeta)
	if err != nil {
		return nil, index, fmt.Errorf("cannot perform blocking Consul API request at %q: %w", path, err)
	}
	return data, index, nil
}
