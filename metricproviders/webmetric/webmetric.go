package webmetric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/utils/evaluate"
	metricutil "github.com/argoproj/argo-rollouts/utils/metric"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/jsonpath"
)

const (
	//ProviderType indicates the provider is prometheus
	ProviderType = "WebMetric"
)

// Provider contains all the required components to run a WebMetric query
// Implements the Provider Interface
type Provider struct {
	logCtx     log.Entry
	client     *http.Client
	jsonParser *jsonpath.JSONPath
}

// Type incidates provider is a WebMetric provider
func (p *Provider) Type() string {
	return ProviderType
}

func (p *Provider) Run(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric) v1alpha1.Measurement {
	var (
		err error
	)

	startTime := metav1.Now()

	// Measurement to pass back
	measurement := v1alpha1.Measurement{
		StartedAt: &startTime,
	}

	// Create request
	request := &http.Request{
		Method: "GET", // TODO maybe make this configurable....also implies we will need body templates
	}

	request.URL, err = url.Parse(metric.Provider.Web.URL)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	for _, header := range metric.Provider.Web.Headers {
		request.Header.Set(header.Key, header.Value)
	}

	// Send Request
	response, err := p.client.Do(request)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	value, status, err := p.parseResponse(metric, response)
	if err != nil || response.StatusCode != http.StatusOK {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	measurement.Value = value
	measurement.Phase = status
	finishedTime := metav1.Now()
	measurement.FinishedAt = &finishedTime

	return measurement
}

func (p *Provider) parseResponse(metric v1alpha1.Metric, response *http.Response) (string, v1alpha1.AnalysisPhase, error) {
	var data interface{}

	bodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Received no bytes in response: %v", err)
	}

	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Could not parse JSON body: %v", err)
	}

	buf := new(bytes.Buffer)
	err = p.jsonParser.Execute(buf, data)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Could not find JSONPath in body: %s", err)
	}
	out := buf.String()

	// Try to get the right primitive
	outInterface := parsePrimitiveFromString(out)

	status := p.evaluateResponse(metric, outInterface)
	return out, status, nil
}

func (p *Provider) evaluateResponse(metric v1alpha1.Metric, result interface{}) v1alpha1.AnalysisPhase {
	successCondition := false
	failCondition := false
	var err error

	if metric.SuccessCondition != "" {
		successCondition, err = evaluate.EvalCondition(result, metric.SuccessCondition)
		if err != nil {
			p.logCtx.Warning(err.Error())
			return v1alpha1.AnalysisPhaseError
		}
	}
	if metric.FailureCondition != "" {
		failCondition, err = evaluate.EvalCondition(result, metric.FailureCondition)
		if err != nil {
			return v1alpha1.AnalysisPhaseError
		}
	}

	switch {
	case metric.SuccessCondition == "" && metric.FailureCondition == "":
		//Always return success unless there is an error
		return v1alpha1.AnalysisPhaseSuccessful
	case metric.SuccessCondition != "" && metric.FailureCondition == "":
		// Without a failure condition, a measurement is considered a failure if the measurement's success condition is not true
		failCondition = !successCondition
	case metric.SuccessCondition == "" && metric.FailureCondition != "":
		// Without a success condition, a measurement is considered a successful if the measurement's failure condition is not true
		successCondition = !failCondition
	}

	if failCondition {
		return v1alpha1.AnalysisPhaseFailed
	}

	if !failCondition && !successCondition {
		return v1alpha1.AnalysisPhaseInconclusive
	}

	// If we reach this code path, failCondition is false and successCondition is true
	return v1alpha1.AnalysisPhaseSuccessful
}

// Resume should not be used the WebMetric provider since all the work should occur in the Run method
func (p *Provider) Resume(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("WebMetric provider should not execute the Resume method")
	return measurement
}

// Terminate should not be used the WebMetric provider since all the work should occur in the Run method
func (p *Provider) Terminate(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("WebMetric provider should not execute the Terminate method")
	return measurement
}

// GarbageCollect is a no-op for the WebMetric provider
func (p *Provider) GarbageCollect(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, limit int) error {
	return nil
}

func NewWebMetricHttpClient(metric v1alpha1.Metric) *http.Client {
	var (
		timeout time.Duration
	)

	// Using a default timeout of 10 seconds
	if metric.Provider.Web.Timeout <= 0 {
		timeout = time.Duration(10) * time.Second
	} else {
		timeout = time.Duration(metric.Provider.Web.Timeout) * time.Second
	}

	c := &http.Client{
		Timeout: timeout,
	}
	return c
}

func NewWebMetricJsonParser(metric v1alpha1.Metric) (*jsonpath.JSONPath, error) {
	jsonParser := jsonpath.New("metrics")

	err := jsonParser.Parse(metric.Provider.Web.JSONPath)

	return jsonParser, err
}

func NewWebMetricProvider(logCtx log.Entry, client *http.Client, jsonParser *jsonpath.JSONPath) *Provider {
	return &Provider{
		logCtx:     logCtx,
		client:     client,
		jsonParser: jsonParser,
	}
}

func parsePrimitiveFromString(in string) interface{} {
	// Chain ordering as follows:
	// int -> float -> bool -> string

	// 64 bit Int conversion
	inAsInt, err := strconv.ParseInt(in, 10, 64)
	if err == nil {
		return inAsInt
	}

	// Float conversion
	inAsFloat, err := strconv.ParseFloat(in, 64)
	if err == nil {
		return inAsFloat
	}

	// Bool conversion
	inAsBool, err := strconv.ParseBool(in)
	if err == nil {
		return inAsBool
	}

	// Else
	return in
}
