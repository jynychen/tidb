// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !codes
// +build !codes

package testdata

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	contextutil "github.com/pingcap/tidb/pkg/util/context"
	"github.com/stretchr/testify/require"
)

// record is a flag used for generate test result.
var record bool

func init() {
	flag.BoolVar(&record, "record", false, "to generate test result")
}

type testCases struct {
	Name       string
	Cases      *json.RawMessage // For delayed parse.
	decodedOut any              // For generate output.
}

// TestData stores all the data of a test suite.
type TestData struct {
	input          []testCases
	output         []testCases
	casout         []testCases
	filePathPrefix string
	funcMap        map[string]int
}

func loadTestSuiteData(dir, suiteName string, cascades ...bool) (res TestData, err error) {
	inCascades := len(cascades) > 0 && cascades[0]
	res.filePathPrefix = filepath.Join(dir, suiteName)
	res.input, err = loadTestSuiteCases(fmt.Sprintf("%s_in.json", res.filePathPrefix))
	if err != nil {
		return res, err
	}

	// Load all test cases result in order to keep the unrelated test results.
	res.output, err = loadTestSuiteCases(fmt.Sprintf("%s_out.json", res.filePathPrefix))
	if err != nil {
		return res, err
	}
	if len(res.input) != len(res.output) {
		return res, fmt.Errorf("Number of test input cases %d does not match test output cases %d", len(res.input), len(res.output))
	}

	if inCascades {
		// Load all test cascades result in order to keep the unrelated test results.
		res.casout, err = loadTestSuiteCases(fmt.Sprintf("%s_xut.json", res.filePathPrefix))
		if err != nil {
			return res, err
		}
		if len(res.input) != len(res.output) {
			return res, fmt.Errorf("Number of test input cases %d does not match test casout cases %d", len(res.input), len(res.casout))
		}
	}

	res.funcMap = make(map[string]int, len(res.input))
	for i, test := range res.input {
		res.funcMap[test.Name] = i
		if test.Name != res.output[i].Name {
			return res, fmt.Errorf("Input name of the %d-case %s does not match output %s", i, test.Name, res.output[i].Name)
		}
		if inCascades {
			if test.Name != res.casout[i].Name {
				return res, fmt.Errorf("Input name of the %d-case %s does not match casout %s", i, test.Name, res.casout[i].Name)
			}
		}
	}
	return res, nil
}

func loadTestSuiteCases(filePath string) (res []testCases, err error) {
	//nolint: gosec
	jsonFile, err := os.Open(filePath)
	if err != nil {
		return res, err
	}
	defer func() {
		if err1 := jsonFile.Close(); err == nil && err1 != nil {
			err = err1
		}
	}()
	byteValue, err := io.ReadAll(jsonFile)
	if err != nil {
		return res, err
	}
	// Remove comments, since they are not allowed in json.
	// Note that this logic is not always correct. For example, you can only add comments in a new line.
	// Appending comments to the end of a line with other content can't be identified.
	// flag:
	//   s: let . match \n
	//   m: let ^/$ match begin/end of a line in addition to begin/end of the entire text
	// ^: begin of line
	// \s*: zero or more whitespace characters
	// .*?: zero or more any characters, but match as few as possible
	re := regexp.MustCompile(`(?sm)^\s*//.*?\n`)
	err = json.Unmarshal(re.ReplaceAll(byteValue, nil), &res)
	return res, err
}

// OnRecord execute the function to update result.
func OnRecord(updateFunc func()) {
	if record {
		updateFunc()
	}
}

// ConvertRowsToStrings converts [][]interface{} to []string.
func ConvertRowsToStrings(rows [][]any) (rs []string) {
	for _, row := range rows {
		s := fmt.Sprintf("%v", row)
		// Trim the leftmost `[` and rightmost `]`.
		s = s[1 : len(s)-1]
		rs = append(rs, s)
	}
	return rs
}

// ConvertSQLWarnToStrings converts []SQLWarn to []string.
func ConvertSQLWarnToStrings(warns []contextutil.SQLWarn) (rs []string) {
	for _, warn := range warns {
		rs = append(rs, fmt.Sprint(warn.Err.Error()))
	}
	return rs
}

// LoadTestCases Loads the test cases for a test function.
// opts[0] should be the cascades "on" or "off"
// opts[1] should be the test portal caller name to locate the output file dir.
func (td *TestData) LoadTestCases(t *testing.T, in any, out any, opts ...string) {
	withCascadesOpts := len(opts) > 0
	inCascades := len(opts) > 0 && opts[0] == "on"
	// Extract caller's name.
	var funcName string
	if !withCascadesOpts {
		pc, _, _, ok := runtime.Caller(1)
		require.True(t, ok)
		details := runtime.FuncForPC(pc)
		funcNameIdx := strings.LastIndex(details.Name(), ".")
		funcName = details.Name()[funcNameIdx+1:]
	} else {
		// since cascades is called in macro wrapper, use the passed caller name
		require.True(t, len(opts) > 1)
		funcName = opts[1]
	}

	casesIdx, ok := td.funcMap[funcName]
	require.Truef(t, ok, "Must get test %s", funcName)
	err := json.Unmarshal(*td.input[casesIdx].Cases, in)
	require.NoError(t, err)
	if !record {
		if !inCascades {
			err = json.Unmarshal(*td.output[casesIdx].Cases, out)
			require.NoError(t, err)
		} else {
			err = json.Unmarshal(*td.casout[casesIdx].Cases, out)
			require.NoError(t, err)
		}
	} else {
		// Init for generate output file.
		inputLen := reflect.ValueOf(in).Elem().Len()
		v := reflect.ValueOf(out).Elem()
		if v.Kind() == reflect.Slice {
			v.Set(reflect.MakeSlice(v.Type(), inputLen, inputLen))
		}
	}
	if !inCascades {
		td.output[casesIdx].decodedOut = out
	} else {
		td.casout[casesIdx].decodedOut = out
	}
}

// LoadTestCasesByName loads the test cases for a test function by its name.
func (td *TestData) LoadTestCasesByName(caseName string, t *testing.T, in any, out any) {
	casesIdx, ok := td.funcMap[caseName]
	require.Truef(t, ok, "Case name: %s", caseName)
	require.NoError(t, json.Unmarshal(*td.input[casesIdx].Cases, in))

	if Record() {
		inputLen := reflect.ValueOf(in).Elem().Len()
		v := reflect.ValueOf(out).Elem()
		if v.Kind() == reflect.Slice {
			v.Set(reflect.MakeSlice(v.Type(), inputLen, inputLen))
		}
	} else {
		require.NoError(t, json.Unmarshal(*td.output[casesIdx].Cases, out))
	}

	td.output[casesIdx].decodedOut = out
}

func (td *TestData) generateOutputIfNeeded() error {
	if !record {
		return nil
	}

	record := func(cascades bool) error {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		isRecord4ThisSuite := false
		output := td.output
		if cascades {
			output = td.casout
		}
		for i, test := range output {
			if test.decodedOut != nil {
				// Only update the results for the related test cases.
				isRecord4ThisSuite = true
				err := enc.Encode(test.decodedOut)
				if err != nil {
					return err
				}
				res := make([]byte, len(buf.Bytes()))
				copy(res, buf.Bytes())
				buf.Reset()
				rm := json.RawMessage(res)
				output[i].Cases = &rm
			}
		}
		// Skip the record for the unrelated test files.
		if !isRecord4ThisSuite {
			return nil
		}
		err := enc.Encode(output)
		if err != nil {
			return err
		}
		suffix := "_out.json"
		if cascades {
			suffix = "_xut.json"
		}
		file, err := os.Create(fmt.Sprintf("%s"+suffix, td.filePathPrefix))
		if err != nil {
			return err
		}
		defer func() {
			if err1 := file.Close(); err == nil && err1 != nil {
				err = err1
			}
		}()
		_, err = file.Write(buf.Bytes())
		return err
	}

	err := record(false)
	if err != nil {
		return err
	}
	return record(true)
}

// BookKeeper does TestData suite bookkeeping.
type BookKeeper map[string]TestData

// LoadTestSuiteData loads test suite data from file and bookkeeping in the map.
func (m *BookKeeper) LoadTestSuiteData(dir, suiteName string, cascades ...bool) {
	testData, err := loadTestSuiteData(dir, suiteName, cascades...)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "testdata: Errors on loading test data from file: %v\n", err)
		os.Exit(1)
	}
	(*m)[suiteName] = testData
}

// GenerateOutputIfNeeded generate the output file from data bookkeeping in the map.
func (m *BookKeeper) GenerateOutputIfNeeded() {
	for _, testData := range *m {
		err := testData.generateOutputIfNeeded()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "testdata: Errors on generating output: %v\n", err)
			os.Exit(1)
		}
	}
}

// Record is a temporary method for testutil to avoid "flag redefined: record" error,
// After we migrate all tests based on former testdata, we should remove testutil and this method.
func Record() bool {
	return record
}
