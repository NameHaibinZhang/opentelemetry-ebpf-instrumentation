// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gpumon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseExtraLabels(t *testing.T) {
	t.Run("empty input yields nil", func(t *testing.T) {
		assert.Nil(t, ParseExtraLabels(""))
		assert.Nil(t, ParseExtraLabels("   "))
	})

	t.Run("plain pairs sorted by key", func(t *testing.T) {
		labels := ParseExtraLabels("team=pay,deployment.environment=prod")
		assert.Equal(t, ExtraLabels{
			{Key: "deployment_environment", Value: "prod"},
			{Key: "team", Value: "pay"},
		}, labels)
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		labels := ParseExtraLabels(" team = pay , region = cn-hangzhou ")
		assert.Equal(t, ExtraLabels{
			{Key: "region", Value: "cn-hangzhou"},
			{Key: "team", Value: "pay"},
		}, labels)
	})

	t.Run("last duplicated key wins", func(t *testing.T) {
		labels := ParseExtraLabels("team=a,team=b")
		assert.Equal(t, ExtraLabels{{Key: "team", Value: "b"}}, labels)
	})

	t.Run("entries without equals or with empty key are skipped", func(t *testing.T) {
		labels := ParseExtraLabels("notapair,=value,valid=yes")
		assert.Equal(t, ExtraLabels{{Key: "valid", Value: "yes"}}, labels)
	})

	t.Run("percent-encoded values are decoded", func(t *testing.T) {
		labels := ParseExtraLabels("team=pay%20team")
		assert.Equal(t, ExtraLabels{{Key: "team", Value: "pay team"}}, labels)
	})

	t.Run("invalid percent escapes keep the raw value", func(t *testing.T) {
		labels := ParseExtraLabels("team=100%")
		assert.Equal(t, ExtraLabels{{Key: "team", Value: "100%"}}, labels)
	})

	t.Run("every invalid key character becomes underscore", func(t *testing.T) {
		labels := ParseExtraLabels("a.b/c=1")
		assert.Equal(t, ExtraLabels{{Key: "a_b_c", Value: "1"}}, labels)
	})

	t.Run("leading digit is prefixed with underscore", func(t *testing.T) {
		labels := ParseExtraLabels("1st=one")
		assert.Equal(t, ExtraLabels{{Key: "_1st", Value: "one"}}, labels)
	})

	t.Run("keys colliding after renaming keep the last value", func(t *testing.T) {
		labels := ParseExtraLabels("a.b=first,a_b=second")
		assert.Equal(t, ExtraLabels{{Key: "a_b", Value: "second"}}, labels)
	})
}

func TestExtraLabelsKeysAndValues(t *testing.T) {
	labels := ExtraLabels{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	assert.Equal(t, []string{"a", "b"}, labels.keys())
	assert.Equal(t, []string{"1", "2"}, labels.values())
	assert.Empty(t, ExtraLabels(nil).keys())
	assert.Empty(t, ExtraLabels(nil).values())
}
