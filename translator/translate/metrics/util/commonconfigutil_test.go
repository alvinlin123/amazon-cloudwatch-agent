package util

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProcessLinuxCommonConfigNoValidMetrics(t *testing.T) {
	var input interface{}
	result := map[string]interface{}{}
	e := json.Unmarshal([]byte(`{
					"resources": [
						"*"
					],
					"measurement": [
						"cpu_usage_idle_dummy",
						"cpu_usage_dummy_nice",
						"dummy_cpu_usage_guest"
					],
					"totalcpu": true,
					"metrics_collection_interval": 1
				}`), &input)
	if e == nil {
		hasValidMetrics := ProcessLinuxCommonConfig(input, "cpu", "", result)
		assert.False(t, hasValidMetrics, "Shouldn't return any valid metrics")
	} else {
		panic(e)
	}
}

func TestProcessLinuxCommonConfigHappy(t *testing.T) {
	var input interface{}
	actualResult := map[string]interface{}{}
	e := json.Unmarshal([]byte(`{
					"resources": [
						"*"
					],
					"measurement": [
						"usage_idle",
						"usage_nice",
						"dummy_cpu_usage_guest"
					],
					"totalcpu": true,
					"metrics_collection_interval": 1
				}`), &input)
	if e == nil {
		hasValidMetrics := ProcessLinuxCommonConfig(input, "cpu", "", actualResult)
		expectedResult := map[string]interface{}{
			"fieldpass": []string{"usage_idle", "usage_nice"},
			"interval":  "1s",
			"tags":      map[string]interface{}{"aws:StorageResolution": "true"},
		}
		assert.True(t, hasValidMetrics, "Should return valid metrics")
		assert.Equal(t, expectedResult, actualResult, "should be equal")
	} else {
		panic(e)
	}
}