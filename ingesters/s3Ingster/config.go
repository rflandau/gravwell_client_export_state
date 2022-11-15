/*************************************************************************
 * Copyright 2020 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gravwell/gravwell/v3/ingest"
	"github.com/gravwell/gravwell/v3/ingest/config"
	"github.com/gravwell/gravwell/v3/ingest/entry"
	"github.com/gravwell/gravwell/v3/ingest/processors"
	"github.com/gravwell/gravwell/v3/timegrinder"
)

type TimeConfig struct {
	Ignore_Timestamps         bool //Just apply the current timestamp to lines as we get them
	Assume_Local_Timezone     bool
	Timezone_Override         string
	Timestamp_Format_Override string //override the timestamp format
}

type bucket struct {
	TimeConfig
	AuthConfig
	Tag_Name        string
	Source_Override string
	File_Filters    []string
	Preprocessor    []string
}

type global struct {
	config.IngestConfig
	State_Store_Location string
}

type cfgReadType struct {
	Global       global
	Bucket       map[string]*bucket
	Preprocessor processors.ProcessorConfig
	TimeFormat   config.CustomTimeFormat
}

type cfgType struct {
	config.IngestConfig
	State_Store_Location string
	Bucket               map[string]*bucket
	Preprocessor         processors.ProcessorConfig
	TimeFormat           config.CustomTimeFormat
}

func GetConfig(path, overlayPath string) (*cfgType, error) {
	//read into the intermediary type to maintain backwards compatibility with the old system
	var cr cfgReadType
	if err := config.LoadConfigFile(&cr, path); err != nil {
		return nil, err
	} else if err = config.LoadConfigOverlays(&cr, overlayPath); err != nil {
		return nil, err
	}
	c := &cfgType{
		IngestConfig:         cr.Global.IngestConfig,
		State_Store_Location: cr.Global.State_Store_Location,
		Bucket:               cr.Bucket,
		Preprocessor:         cr.Preprocessor,
		TimeFormat:           cr.TimeFormat,
	}

	if err := verifyConfig(c); err != nil {
		return nil, err
	}
	if c.State_Store_Location == `` {
		return nil, errors.New("Missing State-Store-Location")
	}

	// Verify and set UUID
	if _, ok := c.IngesterUUID(); !ok {
		id := uuid.New()
		if err := c.SetIngesterUUID(id, path); err != nil {
			return nil, err
		}
		if id2, ok := c.IngesterUUID(); !ok || id != id2 {
			return nil, errors.New("Failed to set a new ingester UUID")
		}
	}
	return c, nil
}

func verifyConfig(c *cfgType) error {
	//verify the global parameters
	if err := c.Verify(); err != nil {
		return err
	}

	if len(c.Bucket) == 0 {
		return errors.New("No buckets specified")
	}

	if err := c.Preprocessor.Validate(); err != nil {
		return err
	} else if err = c.TimeFormat.Validate(); err != nil {
		return err
	}

	for k, v := range c.Bucket {
		if len(v.Tag_Name) == 0 {
			v.Tag_Name = entry.DefaultTagName
		}
		if strings.ContainsAny(v.Tag_Name, ingest.FORBIDDEN_TAG_SET) {
			return errors.New("Invalid characters in the Tag-Name for " + k)
		}
		if v.Timezone_Override != "" {
			if v.Assume_Local_Timezone {
				// cannot do both
				return fmt.Errorf("Cannot specify Assume-Local-Timezone and Timezone-Override in the same listener %v", k)
			}
			if _, err := time.LoadLocation(v.Timezone_Override); err != nil {
				return fmt.Errorf("Invalid timezone override %v in listener %v: %v", v.Timezone_Override, k, err)
			}
		}
		if v.Source_Override != `` {
			if net.ParseIP(v.Source_Override) == nil {
				return fmt.Errorf("Source-Override %s is not a valid IP address", v.Source_Override)
			}
		}

		if err := c.Preprocessor.CheckProcessors(v.Preprocessor); err != nil {
			return fmt.Errorf("Listener %s preprocessor invalid: %v", k, err)
		}
		if err := v.AuthConfig.validate(); err != nil {
			return err
		}
	}

	return nil
}

func (c *cfgType) Tags() ([]string, error) {
	var tags []string
	tagMp := make(map[string]bool, 1)

	for _, v := range c.Bucket {
		if len(v.Tag_Name) == 0 {
			continue
		}
		if _, ok := tagMp[v.Tag_Name]; !ok {
			tags = append(tags, v.Tag_Name)
			tagMp[v.Tag_Name] = true
		}
	}

	if len(tags) == 0 {
		return nil, errors.New("No tags specified")
	}
	sort.Strings(tags)
	return tags, nil
}

func (c *cfgType) IngestBaseConfig() config.IngestConfig {
	return c.IngestConfig
}

func (c *cfgType) newTimeGrinder(tc TimeConfig) (tg *timegrinder.TimeGrinder, err error) {
	tcfg := timegrinder.Config{
		EnableLeftMostSeed: true,
	}
	if tg, err = timegrinder.NewTimeGrinder(tcfg); err != nil {
		err = fmt.Errorf("failed to get a handle on the timegrinder %w", err)
		return
	} else if err = c.TimeFormat.LoadFormats(tg); err != nil {
		err = fmt.Errorf("failed to load custom time formats %w", err)
		return
	}
	if tc.Assume_Local_Timezone {
		tg.SetLocalTime()
	}
	if tc.Timezone_Override != `` {
		if err = tg.SetTimezone(tc.Timezone_Override); err != nil {
			err = fmt.Errorf("failed to set timezone to %v: %v", tc.Timezone_Override, err)
			return
		}
	}
	if tc.Timestamp_Format_Override != `` {
		if err = tg.SetFormatOverride(tc.Timestamp_Format_Override); err != nil {
			return
		}
	}
	return
}

func (g *global) Verify() (err error) {
	if err = g.IngestConfig.Verify(); err != nil {
		return
	}
	err = g.verifyStateStore()
	return
}

func (g *global) StatePath() string {
	return g.State_Store_Location
}

func (g *global) verifyStateStore() (err error) {
	if g.State_Store_Location == `` {
		g.State_Store_Location = defaultStateLoc
	}
	return
}

func (tc TimeConfig) validate() (err error) {
	//TODO FIXME check the timezone override and maybe the timestamp format override
	return
}