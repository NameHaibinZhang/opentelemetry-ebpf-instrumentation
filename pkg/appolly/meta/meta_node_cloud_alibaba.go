// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package meta // import "go.opentelemetry.io/obi/pkg/appolly/meta"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
)

// Alibaba Cloud ECS IMDS base URL. It is a variable so tests can point it to
// a mock server, like the connectionTimeout variable.
var alibabaIMDSBaseURL = "http://100.100.100.200/latest"

const (
	alibabaIMDSTokenPath = "/api/token"
	alibabaIMDSMetaPath  = "/meta-data/"

	// the session token is requested with a PUT request carrying its TTL,
	// and sent back in every metadata request
	alibabaIMDSTokenTTLHeader = "X-aliyun-ecs-metadata-token-ttl-seconds"
	alibabaIMDSTokenHeader    = "X-aliyun-ecs-metadata-token"
	alibabaIMDSTokenTTL       = "21600"

	// metadata values are tiny: cap the response size to protect against a
	// misbehaving endpoint
	alibabaIMDSMaxResponseSize = 1 << 20
)

// errNotAlibabaCloud marks failures meaning that OBI is not running in an
// Alibaba Cloud ECS instance, so querying the IMDS is not retriable.
var errNotAlibabaCloud = errors.New("not running in Alibaba Cloud")

// metadata fields exposed by the Alibaba Cloud ECS IMDS.
// The instance hostname is not fetched: OBI discards the host.name attribute
// during the node metadata merge anyway.
var alibabaIMDSFields = []struct {
	key  attr.Name
	path string
}{
	{attr.Name(semconv.CloudAccountIDKey), "owner-account-id"},
	{attr.Name(semconv.CloudRegionKey), "region-id"},
	{attr.Name(semconv.CloudAvailabilityZoneKey), "zone-id"},
	{attr.Name(semconv.HostImageIDKey), "image-id"},
	{attr.Name(semconv.HostTypeKey), "instance/instance-type"},
}

// alibabaCloudNodeFetcher queries the Alibaba Cloud ECS instance metadata
// service. Unlike the other cloud providers, the Alibaba IMDS requires a
// session token, obtained with a PUT request and sent in every metadata
// request.
func alibabaCloudNodeFetcher() fetcher {
	log := slog.With("component", "meta.NodeMeta.alibabaCloudNodeFetcher")
	return func(ctx context.Context) (NodeMeta, error) {
		// we expect very short response time in a cloud environment
		ctx, cancel := context.WithTimeout(ctx, connectionTimeout)
		defer cancel()

		meta, err := fetchAlibabaCloudMetadata(ctx)
		switch {
		case errors.Is(err, errNotAlibabaCloud):
			log.Debug("Alibaba Cloud IMDS not available", "error", err)
			return NodeMeta{}, nil
		case err != nil:
			return NodeMeta{}, err
		}
		// instance-id is the closest equivalent to a cloud-wide host identifier
		log.Info("detected Cloud metadata")
		log.Debug("cloud metadata", "metadata", fmt.Sprintf("%+v", meta))
		return meta, nil
	}
}

func fetchAlibabaCloudMetadata(ctx context.Context) (NodeMeta, error) {
	token, err := alibabaIMDSRequest(ctx, http.MethodPut, alibabaIMDSBaseURL+alibabaIMDSTokenPath, "")
	if err != nil {
		return NodeMeta{}, err
	}

	meta := NodeMeta{Metadata: make([]Entry, 0, len(alibabaIMDSFields)+2)}
	meta.Metadata = append(meta.Metadata,
		Entry{
			Key:   attr.Name(semconv.CloudProviderKey),
			Value: semconv.CloudProviderAlibabaCloud.Value.AsString(),
		},
		Entry{
			Key:   attr.Name(semconv.CloudPlatformKey),
			Value: semconv.CloudPlatformAlibabaCloudECS.Value.AsString(),
		},
	)
	for _, field := range alibabaIMDSFields {
		value, err := alibabaIMDSRequest(ctx, http.MethodGet, alibabaIMDSBaseURL+alibabaIMDSMetaPath+field.path, token)
		if err != nil {
			return NodeMeta{}, err
		}
		meta.Metadata = append(meta.Metadata, Entry{Key: field.key, Value: value})
	}

	hostID, err := alibabaIMDSRequest(ctx, http.MethodGet, alibabaIMDSBaseURL+alibabaIMDSMetaPath+"instance-id", token)
	if err != nil {
		return NodeMeta{}, err
	}
	meta.HostID = hostID
	return meta, nil
}

func alibabaIMDSRequest(ctx context.Context, method, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return "", err
	}
	if token == "" {
		req.Header.Set(alibabaIMDSTokenTTLHeader, alibabaIMDSTokenTTL)
	} else {
		req.Header.Set(alibabaIMDSTokenHeader, token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// the IMDS is not reachable: OBI is not running in Alibaba Cloud
		return "", fmt.Errorf("%w: %w", errNotAlibabaCloud, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			// temporary IMDS failure: retriable
			return "", fmt.Errorf("IMDS returned %q for %s", resp.Status, url)
		}
		return "", fmt.Errorf("%w: IMDS returned %q for %s", errNotAlibabaCloud, resp.Status, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, alibabaIMDSMaxResponseSize))
	if err != nil {
		return "", fmt.Errorf("reading Alibaba Cloud IMDS response for %s: %w", url, err)
	}
	return strings.TrimSpace(string(body)), nil
}
