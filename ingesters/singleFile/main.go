/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
	"unicode/utf8"

	"github.com/gravwell/gravwell/v3/ingest"
	"github.com/gravwell/gravwell/v3/ingest/config"
	"github.com/gravwell/gravwell/v3/ingest/entry"
	"github.com/gravwell/ingesters/v3/args"
	"github.com/gravwell/ingesters/v3/utils"
	"github.com/gravwell/ingesters/v3/version"
	"github.com/gravwell/timegrinder/v3"
)

const (
	initBuffSize = 4 * 1024 * 1024
	maxBuffSize  = 128 * 1024 * 1024
)

var (
	tso         = flag.String("timestamp-override", "", "Timestamp override")
	tzo         = flag.String("timezone-override", "", "Timezone override e.g. America/Chicago")
	inFile      = flag.String("i", "", "Input file to process (specify - for stdin)")
	ver         = flag.Bool("v", false, "Print version and exit")
	utc         = flag.Bool("utc", false, "Assume UTC time")
	ignoreTS    = flag.Bool("ignore-ts", false, "Ignore timetamp")
	ignorePfx   = flag.String("ignore-prefix", "", "Ignore lines that start with the prefix")
	verbose     = flag.Bool("verbose", false, "Print every step")
	quotable    = flag.Bool("quotable-lines", false, "Allow lines to contain quoted newlines")
	cleanQuotes = flag.Bool("clean-quotes", false, "clean quotes off lines")
	blockSize   = flag.Int("block-size", 0, "Optimized ingest using blocks, 0 disables")
	status      = flag.Bool("status", false, "Output ingest rate stats as we go")
	srcOvr      = flag.String("source-override", "", "Override source with address, hash, or integeter")

	nlBytes          = []byte("\n")
	count            uint64
	totalBytes       uint64
	dur              time.Duration
	noTg             bool //no timegrinder
	bsize            int
	ignorePrefixFlag bool
	ignorePrefix     []byte
	srcOverride      net.IP
)

func init() {
	flag.Parse()
	if *ver {
		version.PrintVersion(os.Stdout)
		ingest.PrintVersion(os.Stdout)
		os.Exit(0)
	}
	if *ignoreTS {
		noTg = true
	}
	if *ignorePfx != `` {
		ignorePrefix = []byte(*ignorePfx)
		ignorePrefixFlag = len(ignorePrefix) > 0
	}
	if *blockSize > 0 {
		bsize = *blockSize
	}
}

func main() {
	if *inFile == "" {
		log.Fatal("Input file path required")
	}
	a, err := args.Parse()
	if err != nil {
		log.Fatalf("Invalid arguments: %v\n", err)
	}
	if len(a.Tags) != 1 {
		log.Fatal("File oneshot only accepts a single tag")
	}

	//resolve the timestmap override if there is one
	if *tso != "" {
		if err = timegrinder.ValidateFormatOverride(*tso); err != nil {
			log.Fatalf("Invalid timestamp override: %v\n", err)
		}
	}

	if *srcOvr != `` {
		if srcOverride, err = config.ParseSource(*srcOvr); err != nil {
			log.Fatalf("Invalid source override")
		}
	}

	//get a handle on the input file with a wrapped decompressor if needed
	var fin io.ReadCloser
	if *inFile == "-" {
		fin = os.Stdin
	} else {
		fin, err = utils.OpenBufferedFileReader(*inFile, 8192)
		if err != nil {
			log.Fatalf("Failed to open %s: %v\n", *inFile, err)
		}
	}

	//fire up a uniform muxer
	igst, err := ingest.NewUniformIngestMuxer(a.Conns, a.Tags, a.IngestSecret, a.TLSPublicKey, a.TLSPrivateKey, "")
	if err != nil {
		log.Fatalf("Failed to create new ingest muxer: %v\n", err)
	}
	if err := igst.Start(); err != nil {
		log.Fatalf("Failed to start ingest muxer: %v\n", err)
	}
	if err := igst.WaitForHot(a.Timeout); err != nil {
		log.Fatalf("Failed to wait for hot connection: %v\n", err)
	}
	tag, err := igst.GetTag(a.Tags[0])
	if err != nil {
		log.Fatalf("Failed to resolve tag %s: %v\n", a.Tags[0], err)
	}

	//go ingest the file
	if err := doIngest(fin, igst, tag, *tso); err != nil {
		log.Fatalf("Failed to ingest file: %v\n", err)
	}

	if err = igst.Sync(a.Timeout); err != nil {
		log.Fatalf("Failed to sync ingest muxer: %v\n", err)
	}
	if err := igst.Close(); err != nil {
		log.Fatalf("Failed to close the ingest muxer: %v\n", err)
	}
	if err := fin.Close(); err != nil {
		log.Fatalf("Failed to close the input file: %v\n", err)
	}
	fmt.Printf("Completed in %v (%s)\n", dur, ingest.HumanSize(totalBytes))
	fmt.Printf("Total Count: %s\n", ingest.HumanCount(count))
	fmt.Printf("Entry Rate: %s\n", ingest.HumanEntryRate(count, dur))
	fmt.Printf("Ingest Rate: %s\n", ingest.HumanRate(totalBytes, dur))
}

func doIngest(fin io.Reader, igst *ingest.IngestMuxer, tag entry.EntryTag, tso string) (err error) {
	//if not doing regular updates, just fire it off
	if !*status {
		err = ingestFile(fin, igst, tag, tso)
		return
	}

	errCh := make(chan error, 1)
	tckr := time.NewTicker(time.Second)
	defer tckr.Stop()
	go func(ch chan error) {
		ch <- ingestFile(fin, igst, tag, tso)
	}(errCh)

loop:
	for {
		lastts := time.Now()
		lastcnt := count
		lastsz := totalBytes
		select {
		case err = <-errCh:
			fmt.Println("\nDONE")
			break loop
		case _ = <-tckr.C:
			dur := time.Since(lastts)
			cnt := count - lastcnt
			bts := totalBytes - lastsz
			fmt.Printf("\r%s %s                                     ",
				ingest.HumanEntryRate(cnt, dur),
				ingest.HumanRate(bts, dur))
		}
	}
	return
}

func ingestFile(fin io.Reader, igst *ingest.IngestMuxer, tag entry.EntryTag, tso string) error {
	var bts []byte
	var ts time.Time
	var ok bool
	var tg *timegrinder.TimeGrinder
	var err error
	var blk []*entry.Entry
	if !noTg {
		//build a new timegrinder
		c := timegrinder.Config{
			EnableLeftMostSeed: true,
			FormatOverride:     tso,
		}

		if tg, err = timegrinder.NewTimeGrinder(c); err != nil {
			return err
		}
		if *utc {
			tg.SetUTC()
		}

		if *tzo != `` {
			if err = tg.SetTimezone(*tzo); err != nil {
				return err
			}
		}
	}

	src := srcOverride
	if src == nil {
		var err error
		if src, err = igst.SourceIP(); err != nil {
			return err
		}
	}

	if bsize > 0 {
		blk = make([]*entry.Entry, 0, bsize)
	}

	scn := bufio.NewScanner(fin)
	if *quotable {
		scn.Split(quotableSplitter)
	}
	scn.Buffer(make([]byte, initBuffSize), maxBuffSize)

	start := time.Now()
	for scn.Scan() {
		if bts = bytes.TrimSuffix(scn.Bytes(), nlBytes); len(bts) == 0 {
			continue
		}
		if *cleanQuotes {
			if bts = trimQuotes(bts); len(bts) == 0 {
				continue
			}
		}
		if ignorePrefixFlag {
			if bytes.HasPrefix(bts, ignorePrefix) {
				continue
			}
		}
		if noTg {
			ts = time.Now()
		} else if ts, ok, err = tg.Extract(bts); err != nil {
			return err
		} else if !ok {
			ts = time.Now()
		}
		ent := &entry.Entry{
			TS:  entry.FromStandard(ts),
			Tag: tag,
			SRC: src,
		}
		ent.Data = append(ent.Data, bts...) //force reallocation due to the scanner
		if bsize == 0 {
			if err = igst.WriteEntry(ent); err != nil {
				return err
			}
		} else {
			blk = append(blk, ent)
			if len(blk) >= bsize {
				if err = igst.WriteBatch(blk); err != nil {
					return err
				}
				blk = make([]*entry.Entry, 0, bsize)
			}
		}
		if *verbose {
			fmt.Println(ent.TS, ent.Tag, ent.SRC, string(ent.Data))
		}
		count++
		totalBytes += uint64(len(ent.Data))
	}
	if len(blk) > 0 {
		if err = igst.WriteBatch(blk); err != nil {
			return err
		}
	}
	dur = time.Since(start)
	return scn.Err()
}

func quotableSplitter(data []byte, atEOF bool) (int, []byte, error) {
	var openQuote bool
	var escaped bool
	var r rune
	var width int
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i += width {
		r, width = utf8.DecodeRune(data[i:])
		if escaped {
			//don't care what the character is, we are skipping it
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
		} else if r == '"' {
			openQuote = !openQuote
		} else if r == '\n' && !openQuote {
			// we have our full newline
			return i + 1, dropCR(data[:i]), nil
		}
	}
	if atEOF {
		return len(data), dropCR(data), nil
	}
	//request more data
	return 0, nil, nil
}

func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}

func trimQuotes(data []byte) []byte {
	if len(data) >= 2 {
		if data[0] == '"' && data[len(data)-1] == '"' {
			data = data[1 : len(data)-1]
		}
	}
	return data
}
