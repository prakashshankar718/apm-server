// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package config

import (
	"time"

	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"

	"github.com/elastic/apm-server/elasticsearch"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
)

// SamplingConfig holds configuration related to sampling.
type SamplingConfig struct {
	// Tail holds tail-sampling configuration.
	Tail TailSamplingConfig `config:"tail"`
}

// TailSamplingConfig holds configuration related to tail-sampling.
type TailSamplingConfig struct {
	Enabled bool `config:"enabled"`

	// Policies holds tail-sampling policies.
	//
	// Policies must include at least one policy that matches all traces, to ensure
	// that dropping non-matching traces is intentional.
	Policies []TailSamplingPolicy `config:"policies"`

	ESConfig              *elasticsearch.Config `config:"elasticsearch"`
	Interval              time.Duration         `config:"interval" validate:"min=1s"`
	IngestRateDecayFactor float64               `config:"ingest_rate_decay" validate:"min=0, max=1"`
	StorageGCInterval     time.Duration         `config:"storage_gc_interval" validate:"min=1s"`
	TTL                   time.Duration         `config:"ttl" validate:"min=1s"`
	StorageLimit          string                `config:"storage_limit"`
	StorageLimitParsed    uint64

	esConfigured bool
}

// TailSamplingPolicy holds a tail-sampling policy.
type TailSamplingPolicy struct {
	// Service holds attributes of the service which this policy matches.
	Service struct {
		Name        string `config:"name"`
		Environment string `config:"environment"`
	} `config:"service"`

	// Trace holds attributes of the trace which this policy matches.
	Trace struct {
		Name    string `config:"name"`
		Outcome string `config:"outcome"`
	} `config:"trace"`

	// SampleRate holds the sample rate applied for this policy.
	SampleRate float64 `config:"sample_rate" validate:"min=0, max=1"`
}

func (c *TailSamplingConfig) Unpack(in *config.C) error {
	type tailSamplingConfig TailSamplingConfig
	cfg := tailSamplingConfig(defaultTailSamplingConfig())
	if err := in.Unpack(&cfg); err != nil {
		return errors.Wrap(err, "error unpacking tail sampling config")
	}
	limit, err := humanize.ParseBytes(cfg.StorageLimit)
	if err != nil {
		return err
	}
	cfg.StorageLimitParsed = limit
	cfg.Enabled = in.Enabled()
	*c = TailSamplingConfig(cfg)
	c.esConfigured = in.HasField("elasticsearch")
	c.StorageLimitParsed = limit
	return errors.Wrap(c.Validate(), "invalid tail sampling config")
}

func (c *TailSamplingConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if len(c.Policies) == 0 {
		return errors.New("no policies specified")
	}
	var anyDefaultPolicy bool
	for _, policy := range c.Policies {
		if policy == (TailSamplingPolicy{SampleRate: policy.SampleRate}) {
			// We have at least one default policy.
			anyDefaultPolicy = true
			break
		}
	}
	if !anyDefaultPolicy {
		return errors.New("no default (empty criteria) policy specified")
	}
	return nil
}

func (c *TailSamplingConfig) setup(log *logp.Logger, outputESCfg *config.C) error {
	if !c.Enabled {
		return nil
	}
	if !c.esConfigured && outputESCfg != nil {
		log.Info("Falling back to elasticsearch output for tail-sampling")
		if err := outputESCfg.Unpack(&c.ESConfig); err != nil {
			return errors.Wrap(err, "error unpacking output.elasticsearch config for tail sampling")
		}
	}
	return nil
}

func defaultSamplingConfig() SamplingConfig {
	tail := defaultTailSamplingConfig()
	return SamplingConfig{
		Tail: tail,
	}
}

func defaultTailSamplingConfig() TailSamplingConfig {
	cfg := TailSamplingConfig{
		Enabled:               false,
		ESConfig:              elasticsearch.DefaultConfig(),
		Interval:              1 * time.Minute,
		IngestRateDecayFactor: 0.25,
		StorageGCInterval:     5 * time.Minute,
		TTL:                   30 * time.Minute,
		StorageLimit:          "3GB",
	}
	parsed, err := humanize.ParseBytes(cfg.StorageLimit)
	if err != nil {
		panic(err)
	}
	cfg.StorageLimitParsed = parsed
	return cfg
}
