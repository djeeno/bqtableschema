package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/iterator"
)

const (
	// optName
	optNameProjectID  = "project"
	optNameDataset    = "dataset"
	optNameKeyFile    = "keyfile"
	optNameOutputFile = "output"
	// envName
	envNameGoogleApplicationCredentials = "GOOGLE_APPLICATION_CREDENTIALS"
	envNameBigQueryDataset              = "BIGQUERY_DATASET"
	envNameOutputFile                   = "OUTPUT_FILE"
	// defaultValue
	defaultValueEmpty      = ""
	defaultValueOutputFile = "bqtableschema/bqtableschema.generated.go"
)

var (
	// optValue
	optValueProjectID  string
	optValueDataset    string
	optValueKeyFile    string
	optValueOutputPath string
)

func init() {
	flag.StringVar(&optValueProjectID, optNameProjectID, defaultValueEmpty, "")
	flag.StringVar(&optValueDataset, optNameDataset, defaultValueEmpty, "")
	flag.StringVar(&optValueKeyFile, optNameKeyFile, defaultValueEmpty, "path to service account json key file")
	flag.StringVar(&optValueOutputPath, optNameOutputFile, defaultValueEmpty, "path to output the generated code")
	flag.Parse()
}

const (
	generatedPackage       = "bqtableschema"
	generatedContentHeader = "// Code generated by go Run github.com/djeeno/bqtableschema/generator.go; DO NOT EDIT.\n\npackage " + generatedPackage + "\n\n"
)

// Run is effectively a `main` function.
// It is separated from the `main` function because of addressing an issue where` defer` is not executed when `os.Exit` is executed.
func Run(ctx context.Context) error {
	var tail string

	keyfile, err := getOptOrEnvOrDefault(optNameKeyFile, optValueKeyFile, envNameGoogleApplicationCredentials, "")
	if err != nil {
		return fmt.Errorf("getOptOrEnvOrDefault: %w", err)
	}
	// set GOOGLE_APPLICATION_CREDENTIALS for Google Cloud SDK
	if os.Getenv(envNameGoogleApplicationCredentials) != keyfile {
		if err := os.Setenv(envNameGoogleApplicationCredentials, keyfile); err != nil {
			return fmt.Errorf("os.Setenv: %w", err)
		}
	}

	cred, err := newGoogleApplicationCredentials(keyfile)
	if err != nil {
		return fmt.Errorf("newGoogleApplicationCredentials: %w", err)
	}

	var projectID string
	if optValueProjectID != "" {
		projectID = optValueProjectID
	} else {
		projectID = cred.ProjectID
	}

	c, err := bigquery.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("bigquery.NewClient: %w", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("c.Close: %v\n", err)
		}
	}()

	dataset, err := getOptOrEnvOrDefault(optNameDataset, optValueDataset, envNameBigQueryDataset, "")
	if err != nil {
		return fmt.Errorf("getOptOrEnvOrDefault: %w", err)
	}

	filePath, err := getOptOrEnvOrDefault(optNameOutputFile, optValueOutputPath, envNameOutputFile, defaultValueOutputFile)
	if err != nil {
		return fmt.Errorf("getOptOrEnvOrDefault: %w", err)
	}

	tables, err := getAllTables(ctx, c, dataset)
	if err != nil {
		return fmt.Errorf("getAllTables: %w", err)
	}

	importPackagesUniq := make(map[string]bool)

	for i, table := range tables {
		structCode, packages, err := generateTableSchemaCode(ctx, table)
		if err != nil {
			log.Printf("generateTableSchemaCode: %v\n", err)
			continue
		}

		for _, pkg := range packages {
			importPackagesUniq[pkg] = true
		}

		tail = tail + structCode

		if !isLastLoop(i, len(tables)) {
			tail = tail + "\n"
		}
	}

	// TODO(djeeno): import packages
	var importsCode string
	switch {
	case len(importPackagesUniq) == 0:
		importsCode = ""
	case len(importPackagesUniq) == 1:
		for pkg := range importPackagesUniq {
			importsCode = "import \"" + pkg + "\"\n"
		}
		importsCode = importsCode + "\n"
	case len(importPackagesUniq) >= 2:
		importsCode = "import (\n"
		importPackagesForSort := make([]string, len(importPackagesUniq))
		idx := 0
		for pkg := range importPackagesUniq {
			importPackagesForSort[idx] = pkg
			idx++
		}
		sort.Strings(importPackagesForSort)
		for _, pkg := range importPackagesForSort {
			importsCode = importsCode + "\t\"" + pkg + "\"\n"
		}
		importsCode = importsCode + ")\n\n"
	}

	// NOTE(djeeno): combine
	generatedCode := generatedContentHeader + importsCode + tail

	if err := mkdirIfNotExist(filepath.Dir(filePath)); err != nil {
		return fmt.Errorf("mkdirIfNotExist: %w", err)
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("os.OpenFile: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("file.Close: %v\n", err)
		}
	}()

	// NOTE(djeeno): output
	if _, err := fmt.Fprint(file, generatedCode); err != nil {
		return fmt.Errorf("fmt.Fprint: %w", err)
	}

	return nil
}

func generateTableSchemaCode(ctx context.Context, table *bigquery.Table) (generatedCode string, packages []string, err error) {
	if len(table.TableID) == 0 {
		return "", nil, fmt.Errorf("*bigquery.Table.TableID is empty. *bigquery.Table struct dump: %#v", table)
	}
	structName := capitalizeInitial(table.TableID)

	md, err := table.Metadata(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("table.Metadata: %w", err)
	}

	// NOTE(djeeno): structs
	generatedCode = fmt.Sprintf("// %s is BigQuery Table (%s) schema struct.\n// Description: %s\ntype %s struct {\n", structName, md.FullID, md.Description, structName)

	schemas := []*bigquery.FieldSchema(md.Schema)

	// NOTE(djeeno): for formats
	var longestNameLength int
	var longestTypeLength int
	for _, schema := range schemas {
		// struct field name length for format
		nameLength := len(schema.Name)
		if longestNameLength < nameLength {
			longestNameLength = nameLength
		}
		goTypeStr, _, err := bigqueryFieldTypeToGoType(schema.Type)
		if err != nil {
			return "", nil, fmt.Errorf("bigqueryFieldTypeToGoType: %w", err)
		}
		// struct field TYPE name length for format
		typeLength := len(goTypeStr)
		if longestTypeLength < typeLength {
			longestTypeLength = typeLength
		}
	}
	format := "\t%-" + strconv.Itoa(longestNameLength) + "s %-" + strconv.Itoa(longestTypeLength) + "s `bigquery:\"%s\"`\n"

	for _, schema := range schemas {
		goTypeStr, pkg, err := bigqueryFieldTypeToGoType(schema.Type)
		if err != nil {
			return "", nil, fmt.Errorf("bigqueryFieldTypeToGoType: %w", err)
		}
		if pkg != "" {
			packages = append(packages, pkg)
		}
		generatedCode = generatedCode + fmt.Sprintf(format, capitalizeInitial(schema.Name), goTypeStr, schema.Name)
	}
	generatedCode = generatedCode + "}\n"

	return generatedCode, packages, nil
}

func mkdirIfNotExist(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.Mkdir(path, 0755)
	}
	return nil
}

func getAllTables(ctx context.Context, c *bigquery.Client, datasetID string) (tables []*bigquery.Table, err error) {
	tableIterator := c.Dataset(datasetID).Tables(ctx)
	for {
		table, err := tableIterator.Next()
		if err != nil {
			if err == iterator.Done {
				break
			}
			return nil, fmt.Errorf("tableIterator.Next: %w", err)
		}
		tables = append(tables, table)
	}
	return tables, nil
}

type googleApplicationCredentials struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
}

func newGoogleApplicationCredentials(path string) (*googleApplicationCredentials, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("os.Open: %w", err)
	}

	bytea, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("ioutil.ReadAll: %w", err)
	}

	cred := googleApplicationCredentials{}
	if err := json.Unmarshal(bytea, &cred); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}

	return &cred, nil
}

func isLastLoop(loopIndex, lengthOfLoop int) bool {
	return loopIndex+1 == lengthOfLoop
}

func getOptOrEnvOrDefault(optKey, optValue, envKey, defaultValue string) (string, error) {
	if optValue != "" {
		return optValue, nil
	}

	envValue := os.Getenv(envKey)
	if envValue != "" {
		return envValue, nil
	}

	if defaultValue != "" {
		return defaultValue, nil
	}

	return "", fmt.Errorf("set option -%s, or set environment variable %s", optKey, envKey)
}

func capitalizeInitial(s string) (capitalized string) {
	if len(s) == 0 {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var (
	typeOfByteSlice = reflect.TypeOf([]byte{})
	typeOfDate      = reflect.TypeOf(civil.Date{})
	typeOfTime      = reflect.TypeOf(civil.Time{})
	typeOfDateTime  = reflect.TypeOf(civil.DateTime{})
	typeOfGoTime    = reflect.TypeOf(time.Time{})
	typeOfRat       = reflect.TypeOf(&big.Rat{})
)

func bigqueryFieldTypeToGoType(bigqueryFieldType bigquery.FieldType) (goType string, pkg string, err error) {
	switch bigqueryFieldType {
	case bigquery.BytesFieldType:
		return typeOfByteSlice.String(), "", nil
	case bigquery.DateFieldType:
		return typeOfDate.String(), "cloud.google.com/go/civil", nil
	case bigquery.TimeFieldType:
		return typeOfTime.String(), "cloud.google.com/go/civil", nil
	case bigquery.DateTimeFieldType:
		return typeOfDateTime.String(), "cloud.google.com/go/civil", nil
	case bigquery.TimestampFieldType:
		return typeOfGoTime.String(), "time", nil
	case bigquery.NumericFieldType:
		return typeOfRat.String(), "math/big", nil
	case bigquery.IntegerFieldType:
		return reflect.Int64.String(), "", nil
	case bigquery.RecordFieldType:
		return "", "", fmt.Errorf("bigquery.FieldType not supported. bigquery.FieldType=%s", bigqueryFieldType)
	case bigquery.GeographyFieldType:
		return reflect.String.String(), "", nil
	case bigquery.StringFieldType:
		return reflect.String.String(), "", nil
	case bigquery.BooleanFieldType:
		return reflect.Bool.String(), "", nil
	case bigquery.FloatFieldType:
		return reflect.Float64.String(), "", nil
	default:
		return "", "", fmt.Errorf("bigquery.FieldType not supported. bigquery.FieldType=%s", bigqueryFieldType)
	}
}

// resolveEnvs resolves environment variables from the arguments passed as environment variable names.
func resolveEnvs(keys ...string) (map[string]string, error) {
	envs := map[string]string{}

	for _, key := range keys {
		envs[key] = os.Getenv(key)
		if envs[key] == "" {
			return nil, fmt.Errorf("resolveEnvs: environment variable %s is empty", key)
		}
	}

	return envs, nil
}

// mergeMap merge map[string]string
func mergeMap(sideToBeMerged, sideToMerge map[string]string) map[string]string {
	m := map[string]string{}

	for k, v := range sideToBeMerged {
		m[k] = v
	}
	for k, v := range sideToMerge {
		m[k] = v
	}
	return (m)
}

func funcName() string {
	pc, _, _, ok := runtime.Caller(1)
	if !ok {
		return ""
	}
	return runtime.FuncForPC(pc).Name()
}

func caller() string {
	pc, file, line, ok := runtime.Caller(1)
	if !ok {
		return fmt.Sprintf("%s[%s:%d]", "null", "null", 0)
	}
	return fmt.Sprintf("%s[%s:%d]", runtime.FuncForPC(pc).Name(), filepath.Base(file), line)
}
