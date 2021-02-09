// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package cloudwatchlogs

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/amazon-cloudwatch-agent/cfg/agentinfo"
	configaws "github.com/aws/amazon-cloudwatch-agent/cfg/aws"
	"github.com/aws/amazon-cloudwatch-agent/handlers"
	"github.com/aws/amazon-cloudwatch-agent/internal"
	"github.com/aws/amazon-cloudwatch-agent/logs"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs"
)

const (
	LogGroupNameTag   = "log_group_name"
	LogStreamNameTag  = "log_stream_name"
	LogTimestampField = "log_timestamp"
	LogEntryField     = "value"

	defaultFlushTimeout = 5 * time.Second
	eventHeaderSize     = 26
	truncatedSuffix     = "[Truncated...]"
	msgSizeLimit        = 256*1024 - eventHeaderSize

	maxRetryTimeout    = 14*24*time.Hour + 10*time.Minute
	metricRetryTimeout = 2 * time.Minute

	attributesInFields = "attributesInFields"
)

type CloudWatchLogs struct {
	Region           string `toml:"region"`
	EndpointOverride string `toml:"endpoint_override"`
	AccessKey        string `toml:"access_key"`
	SecretKey        string `toml:"secret_key"`
	RoleARN          string `toml:"role_arn"`
	Profile          string `toml:"profile"`
	Filename         string `toml:"shared_credential_file"`
	Token            string `toml:"token"`

	//log group and stream names
	LogStreamName string `toml:"log_stream_name"`
	LogGroupName  string `toml:"log_group_name"`

	ForceFlushInterval internal.Duration `toml:"force_flush_interval"` // unit is second

	Log telegraf.Logger `toml:"-"`

	cwDests map[Target]*cwDest
}

func (c *CloudWatchLogs) Connect() error {
	return nil
}

func (c *CloudWatchLogs) Close() error {
	for _, d := range c.cwDests {
		d.Stop()
	}
	return nil
}

func (c *CloudWatchLogs) Write(metrics []telegraf.Metric) error {
	for _, m := range metrics {
		c.writeMetricAsStructuredLog(m)
	}
	return nil
}

func (c *CloudWatchLogs) CreateDest(group, stream string) logs.LogDest {
	if group == "" {
		group = c.LogGroupName
	}
	if stream == "" {
		stream = c.LogStreamName
	}

	t := Target{
		Group:  group,
		Stream: stream,
	}
	return c.getDest(t)
}

func (c *CloudWatchLogs) getDest(t Target) *cwDest {
	if cwd, ok := c.cwDests[t]; ok {
		return cwd
	}

	credentialConfig := &configaws.CredentialConfig{
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		RoleARN:   c.RoleARN,
		Profile:   c.Profile,
		Filename:  c.Filename,
		Token:     c.Token,
	}

	client := cloudwatchlogs.New(
		credentialConfig.Credentials(),
		&aws.Config{
			Endpoint: aws.String(c.EndpointOverride),
			LogLevel: aws.LogLevel(aws.LogDebugWithRequestErrors),
		},
	)
	client.Handlers.Build.PushBackNamed(handlers.NewRequestCompressionHandler([]string{"PutLogEvents"}))
	client.Handlers.Build.PushBackNamed(handlers.NewCustomHeaderHandler("User-Agent", agentinfo.UserAgent()))

	pusher := NewPusher(t, client, c.ForceFlushInterval.Duration, maxRetryTimeout, c.Log)
	cwd := &cwDest{pusher: pusher}
	c.cwDests[t] = cwd
	return cwd
}

func (c *CloudWatchLogs) writeMetricAsStructuredLog(m telegraf.Metric) {
	t, err := c.getTargetFromMetric(m)
	if err != nil {
		c.Log.Errorf("Failed to find target: %v", err)
	}
	cwd := c.getDest(t)
	if cwd == nil {
		c.Log.Warnf("unable to find log destination, group: %v, stream: %v", t.Group, t.Stream)
		return
	}
	cwd.switchToEMF()
	cwd.pusher.RetryDuration = metricRetryTimeout

	e := c.getLogEventFromMetric(m)
	if e == nil {
		return
	}

	cwd.AddEvent(e)
}

func (c *CloudWatchLogs) getTargetFromMetric(m telegraf.Metric) (Target, error) {
	tags := m.Tags()
	logGroup, ok := tags[LogGroupNameTag]
	if !ok {
		return Target{}, fmt.Errorf("structuredlog receive a metric with name '%v' without log group name", m.Name())
	} else {
		m.RemoveTag(LogGroupNameTag)
	}

	logStream, ok := tags[LogStreamNameTag]
	if ok {
		m.RemoveTag(LogStreamNameTag)
	} else if logStream == "" {
		logStream = c.LogStreamName
	}

	return Target{logGroup, logStream}, nil
}

func (c *CloudWatchLogs) getLogEventFromMetric(metric telegraf.Metric) *structuredLogEvent {
	var message string
	if metric.HasField(LogEntryField) {
		var ok bool
		if message, ok = metric.Fields()[LogEntryField].(string); !ok {
			c.Log.Warnf("The log entry value field is not string type: %v", metric.Fields())
			return nil
		}
	} else {
		content := map[string]interface{}{}
		tags := metric.Tags()
		// build all the attributesInFields
		if val, ok := tags[attributesInFields]; ok {
			attributes := strings.Split(val, ",")
			mFields := metric.Fields()
			for _, attr := range attributes {
				if fieldVal, ok := mFields[attr]; ok {
					content[attr] = fieldVal
					metric.RemoveField(attr)
				}
			}
			metric.RemoveTag(attributesInFields)
			delete(tags, attributesInFields)
		}

		// build remaining attributes
		for k := range tags {
			content[k] = tags[k]
		}

		for k, v := range metric.Fields() {
			var value interface{}

			switch t := v.(type) {
			case int:
				value = float64(t)
			case int32:
				value = float64(t)
			case int64:
				value = float64(t)
			case uint:
				value = float64(t)
			case uint32:
				value = float64(t)
			case uint64:
				value = float64(t)
			case float64:
				value = t
			case bool:
				value = t
			case string:
				value = t
			case time.Time:
				value = float64(t.Unix())

			default:
				c.Log.Errorf("Detected unexpected fields (%s,%v) when encoding structured log event, value type %T is not supported", k, v, v)
				return nil
			}
			content[k] = value
		}

		jsonMap, err := json.Marshal(content)
		if err != nil {
			c.Log.Errorf("Unalbe to marshal structured log content: %v", err)
		}
		message = string(jsonMap)
	}

	return &structuredLogEvent{
		msg: message,
		t:   metric.Time(),
	}
}

type structuredLogEvent struct {
	msg string
	t   time.Time
}

func (e *structuredLogEvent) Message() string {
	return e.msg
}

func (e *structuredLogEvent) Time() time.Time {
	return e.t
}

func (e *structuredLogEvent) Done() {}

type cwDest struct {
	*pusher
	sync.Mutex
	isEMF   bool
	stopped bool
}

func (cd *cwDest) Publish(events []logs.LogEvent) error {
	for _, e := range events {
		if !cd.isEMF {
			msg := e.Message()
			if strings.HasPrefix(msg, "{") && strings.HasSuffix(msg, "}") && strings.Contains(msg, "\"CloudWatchMetrics\"") {
				cd.switchToEMF()
			}
		}
		cd.AddEvent(e)
	}
	if cd.stopped {
		return logs.ErrOutputStopped
	}
	return nil
}

func (cd *cwDest) Stop() {
	cd.pusher.Stop()
	cd.stopped = true
}

func (cd *cwDest) AddEvent(e logs.LogEvent) {
	// Drop events for metric path logs when queue is full
	if cd.isEMF {
		cd.pusher.AddEventNonBlocking(e)
	} else {
		cd.pusher.AddEvent(e)
	}
}

func (cd *cwDest) switchToEMF() {
	cd.Lock()
	defer cd.Unlock()
	if !cd.isEMF {
		cd.isEMF = true
		cwl, ok := cd.Service.(*cloudwatchlogs.CloudWatchLogs)
		if ok {
			cwl.Handlers.Build.PushBackNamed(handlers.NewCustomHeaderHandler("x-amzn-logs-format", "json/emf"))
		}
	}
}

func (cd *cwDest) setRetryer(r request.Retryer) {
	cwl, ok := cd.Service.(*cloudwatchlogs.CloudWatchLogs)
	if ok {
		cwl.Retryer = r
	}
}

type Target struct {
	Group, Stream string
}

// Description returns a one-sentence description on the Output
func (c *CloudWatchLogs) Description() string {
	return "Configuration for AWS CloudWatchLogs output."
}

var sampleConfig = `
  ## Amazon REGION
  region = "us-east-1"

  ## Amazon Credentials
  ## Credentials are loaded in the following order
  ## 1) Assumed credentials via STS if role_arn is specified
  ## 2) explicit credentials from 'access_key' and 'secret_key'
  ## 3) shared profile from 'profile'
  ## 4) environment variables
  ## 5) shared credentials file
  ## 6) EC2 Instance Profile
  #access_key = ""
  #secret_key = ""
  #token = ""
  #role_arn = ""
  #profile = ""
  #shared_credential_file = ""

  # The log stream name.
  log_stream_name = "<log_stream_name>"
`

// SampleConfig returns the default configuration of the Output
func (c *CloudWatchLogs) SampleConfig() string {
	return sampleConfig
}

func init() {
	outputs.Add("cloudwatchlogs", func() telegraf.Output {
		return &CloudWatchLogs{
			ForceFlushInterval: internal.Duration{Duration: defaultFlushTimeout},
			cwDests:            make(map[Target]*cwDest),
		}
	})
}
