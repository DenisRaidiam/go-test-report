package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	htmlTemplate "html/template"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	textTemplate "text/template"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

type (
	goTestOutputRow struct {
		Time     string
		TestName string `json:"Test"`
		Action   string
		Package  string
		Elapsed  float64
		Output   string
	}

	testStatus struct {
		TestName           string
		Package            string
		ElapsedTime        float64
		Output             []string
		Passed             bool
		Skipped            bool
		TestFileName       string
		TestFunctionDetail testFunctionFilePos
	}

	templateData struct {
		TestResults        []*testGroupData
		JsIntegrity        htmlTemplate.HTMLAttr
		CssIntegrity       htmlTemplate.HTMLAttr
		NumOfTestPassed    int
		NumOfTestFailed    int
		NumOfTestSkipped   int
		NumOfTests         int
		TestDuration       time.Duration
		ReportTitle        string
		numOfTestsPerGroup int
		OutputFilename     string
		TestExecutionDate  string
	}

	cssTemplateData struct {
		TestResultGroupIndicatorWidth  string
		TestResultGroupIndicatorHeight string
	}

	jsTemplateData struct {
		TestResultsJson string
		JsCode          string
	}

	testGroupData struct {
		FailureIndicator string
		SkippedIndicator string
		TestResults      []*testStatus
	}

	cmdFlags struct {
		titleFlag  string
		sizeFlag   string
		groupSize  int
		listFlag   string
		outputFlag string
		verbose    bool
	}

	goListJSONModule struct {
		Path string
		Dir  string
		Main bool
	}

	goListJSON struct {
		Dir         string
		ImportPath  string
		Name        string
		GoFiles     []string
		TestGoFiles []string
		Module      goListJSONModule
	}

	testFunctionFilePos struct {
		Line int
		Col  int
	}

	testFileDetail struct {
		FileName            string
		TestFunctionFilePos testFunctionFilePos
	}

	testFileDetailsByTest    map[string]*testFileDetail
	testFileDetailsByPackage map[string]testFileDetailsByTest
)

func main() {
	rootCmd, _, _, _ := initRootCommand()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func initRootCommand() (*cobra.Command, *templateData, *cssTemplateData, *cmdFlags) {
	flags := &cmdFlags{}
	tmplData := &templateData{}
	cssTemplateData := &cssTemplateData{}
	rootCmd := &cobra.Command{
		Use:  "go-test-report",
		Long: "Captures go test output via stdin and parses it into a single self-contained html file.",
		RunE: func(cmd *cobra.Command, args []string) (e error) {
			startTime := time.Now()
			if err := parseSizeFlag(cssTemplateData, flags); err != nil {
				return err
			}

			cssTemplate := textTemplate.New("style.css.template")
			cssTemplateStr, err := hex.DecodeString(testReportCssTemplate)
			if err != nil {
				return err
			}
			cssTemplate, err = cssTemplate.Parse(string(cssTemplateStr))

			if err := writeCssFile(cssTemplateData, cssTemplate); err != nil {
				return err
			}

			tmplData.numOfTestsPerGroup = flags.groupSize
			tmplData.ReportTitle = flags.titleFlag
			tmplData.OutputFilename = flags.outputFlag
			if err := checkIfStdinIsPiped(); err != nil {
				return err
			}
			stdin := os.Stdin
			stdinScanner := bufio.NewScanner(stdin)
			defer func() {
				_ = stdin.Close()
			}()
			startTestTime := time.Now()
			allPackageNames, allTests, err := readTestDataFromStdIn(stdinScanner, flags, cmd)
			if err != nil {
				return errors.New(err.Error() + "\n")
			}
			elapsedTestTime := time.Since(startTestTime)
			// used to the location of test functions in test go files by package and test function name.
			var testFileDetailByPackage testFileDetailsByPackage
			if flags.listFlag != "" {
				testFileDetailByPackage, err = getAllDetails(flags.listFlag)
			} else {
				testFileDetailByPackage, err = getPackageDetails(allPackageNames)
			}
			if err != nil {
				return err
			}

			if err = generateReport(tmplData, allTests, testFileDetailByPackage, elapsedTestTime); err != nil {
				return err
			}

			if _, err := cmd.OutOrStdout().Write([]byte(fmt.Sprintf("[go-test-report] finished in %s\n", time.Since(startTime)))); err != nil {
				return err
			}

			return nil
		},
	}
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Prints the version number of go-test-report",
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := fmt.Sprintf("go-test-report v%s", version)
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), msg); err != nil {
				return err
			}
			return nil
		},
	}
	rootCmd.AddCommand(versionCmd)
	rootCmd.PersistentFlags().StringVarP(&flags.titleFlag,
		"title",
		"t",
		"go-test-report",
		"the title text shown in the test report")
	rootCmd.PersistentFlags().StringVarP(&flags.sizeFlag,
		"size",
		"s",
		"24",
		"the size (in pixels) of the clickable indicator for test result groups")
	rootCmd.PersistentFlags().IntVarP(&flags.groupSize,
		"groupSize",
		"g",
		20,
		"the number of tests per test group indicator")
	rootCmd.PersistentFlags().StringVarP(&flags.listFlag,
		"list",
		"l",
		"",
		"the JSON module list")
	rootCmd.PersistentFlags().StringVarP(&flags.outputFlag,
		"output",
		"o",
		"test_report.html",
		"the HTML output file")
	rootCmd.PersistentFlags().BoolVarP(&flags.verbose,
		"verbose",
		"v",
		false,
		"while processing, show the complete output from go test ")

	return rootCmd, tmplData, cssTemplateData, flags
}

func readTestDataFromStdIn(stdinScanner *bufio.Scanner, flags *cmdFlags, cmd *cobra.Command) (allPackageNames map[string]*types.Nil, allTests map[string]*testStatus, e error) {
	allTests = map[string]*testStatus{}
	allPackageNames = map[string]*types.Nil{}

	// read from stdin and parse "go test" results
	for stdinScanner.Scan() {
		lineInput := stdinScanner.Bytes()
		if flags.verbose {
			newline := []byte("\n")
			if _, err := cmd.OutOrStdout().Write(append(lineInput, newline[0])); err != nil {
				return nil, nil, err
			}
		}
		goTestOutputRow := &goTestOutputRow{}
		if err := json.Unmarshal(lineInput, goTestOutputRow); err != nil {
			return nil, nil, err
		}
		if goTestOutputRow.TestName != "" {
			var status *testStatus
			key := goTestOutputRow.Package + "." + goTestOutputRow.TestName
			if _, exists := allTests[key]; !exists {
				status = &testStatus{
					TestName: goTestOutputRow.TestName,
					Package:  goTestOutputRow.Package,
					Output:   []string{},
				}
				allTests[key] = status
			} else {
				status = allTests[key]
			}
			if goTestOutputRow.Action == "pass" || goTestOutputRow.Action == "fail" || goTestOutputRow.Action == "skip" {
				if goTestOutputRow.Action == "pass" {
					status.Passed = true
				}
				if goTestOutputRow.Action == "skip" {
					status.Skipped = true
				}
				status.ElapsedTime = goTestOutputRow.Elapsed
			}
			allPackageNames[goTestOutputRow.Package] = nil
			if strings.Contains(goTestOutputRow.Output, "--- PASS:") {
				goTestOutputRow.Output = strings.TrimSpace(goTestOutputRow.Output)
			}
			status.Output = append(status.Output, goTestOutputRow.Output)
		}
	}
	return allPackageNames, allTests, nil
}

func getAllDetails(listFile string) (testFileDetailsByPackage, error) {
	testFileDetailByPackage := testFileDetailsByPackage{}
	f, err := os.Open(listFile)
	defer f.Close()
	if err != nil {
		return nil, err
	}
	list := json.NewDecoder(f)
	for list.More() {
		goListJSON := goListJSON{}
		if err := list.Decode(&goListJSON); err != nil {
			return nil, err
		}
		packageName := goListJSON.ImportPath
		testFileDetailsByTest, err := getFileDetails(&goListJSON)
		if err != nil {
			return nil, err
		}
		testFileDetailByPackage[packageName] = testFileDetailsByTest
	}
	return testFileDetailByPackage, nil
}

func getPackageDetails(allPackageNames map[string]*types.Nil) (testFileDetailsByPackage, error) {
	var testFileDetailByPackage testFileDetailsByPackage
	ctx := context.Background()
	g, ctx := errgroup.WithContext(ctx)
	details := make(chan testFileDetailsByPackage)
	for packageName := range allPackageNames {
		name := packageName
		g.Go(func() error {
			testFileDetailsByTest, err := getTestDetails(name)
			if err != nil {
				return err
			}
			select {
			case details <- testFileDetailsByPackage{name: testFileDetailsByTest}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil

		})
	}
	go func() {
		g.Wait()
		close(details)
	}()

	testFileDetailByPackage = make(testFileDetailsByPackage, len(allPackageNames))
	for d := range details {
		for packageName, testFileDetailsByTest := range d {
			testFileDetailByPackage[packageName] = testFileDetailsByTest
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return testFileDetailByPackage, nil
}

func getTestDetails(packageName string) (testFileDetailsByTest, error) {
	var out bytes.Buffer
	var cmd *exec.Cmd
	stringReader := strings.NewReader("")
	cmd = exec.Command("go", "list", "-json", packageName)
	cmd.Stdin = stringReader
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	goListJSON := &goListJSON{}
	if err := json.Unmarshal(out.Bytes(), goListJSON); err != nil {
		return nil, err
	}
	return getFileDetails(goListJSON)
}

func getFileDetails(goListJSON *goListJSON) (testFileDetailsByTest, error) {
	testFileDetailByTest := map[string]*testFileDetail{}
	for _, file := range goListJSON.TestGoFiles {
		sourceFilePath := fmt.Sprintf("%s/%s", goListJSON.Dir, file)
		fileSet := token.NewFileSet()
		f, err := parser.ParseFile(fileSet, sourceFilePath, nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				testFileDetail := &testFileDetail{}
				fileSetPos := fileSet.Position(n.Pos())
				folders := strings.Split(fileSetPos.String(), "/")
				fileNameWithPos := folders[len(folders)-1]
				fileDetails := strings.Split(fileNameWithPos, ":")
				lineNum, _ := strconv.Atoi(fileDetails[1])
				colNum, _ := strconv.Atoi(fileDetails[2])
				testFileDetail.FileName = fileDetails[0]
				testFileDetail.TestFunctionFilePos = testFunctionFilePos{
					Line: lineNum,
					Col:  colNum,
				}
				testFileDetailByTest[x.Name.Name] = testFileDetail
			}
			return true
		})
	}
	return testFileDetailByTest, nil
}

type testRef struct {
	key  string
	name string
}
type byName []testRef

func (t byName) Len() int {
	return len(t)
}
func (t byName) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}
func (t byName) Less(i, j int) bool {
	return t[i].name < t[j].name
}

func generateReport(tmplData *templateData, allTests map[string]*testStatus, testFileDetailByPackage testFileDetailsByPackage, elapsedTestTime time.Duration) error {
	// read the html template from the generated embedded asset go file
	tpl := htmlTemplate.New("test_report.html.template")
	testReportHTMLTemplateStr, err := hex.DecodeString(testReportHTMLTemplate)
	if err != nil {
		return err
	}
	tpl, err = tpl.Parse(string(testReportHTMLTemplateStr))
	if err != nil {
		return err
	}

	// read the js template from the generated embedded asset go file

	jsTpl := textTemplate.New("test_report.js.template")
	testReportJsTemplateStr, err := hex.DecodeString(testReportJsTemplate)
	if err != nil {
		return err
	}

	jsTpl, err = jsTpl.Parse(string(testReportJsTemplateStr))
	if err != nil {
		return err
	}

	// read Javascript code from the generated embedded asset go file
	testReportJsCodeStr, err := hex.DecodeString(testReportJsCode)
	if err != nil {
		return err
	}

	jsTemplateData := &jsTemplateData{}
	jsTemplateData.JsCode = string(testReportJsCodeStr)

	tmplData.NumOfTestPassed = 0
	tmplData.NumOfTestFailed = 0
	tmplData.NumOfTestSkipped = 0
	tgCounter := 0
	tgID := 0

	// sort the allTests map by test name (this will produce a consistent order when iterating through the map)
	var tests []testRef
	var testResults []*testGroupData
	for test, status := range allTests {
		tests = append(tests, testRef{test, status.TestName})
	}
	sort.Sort(byName(tests))
	for _, test := range tests {
		status := allTests[test.key]
		if len(testResults) == tgID {
			testResults = append(testResults, &testGroupData{})
		}
		// add file info(name and position; line and col) associated with the test function
		testFileInfo := testFileDetailByPackage[status.Package][status.TestName]
		if testFileInfo != nil {
			status.TestFileName = testFileInfo.FileName
			status.TestFunctionDetail = testFileInfo.TestFunctionFilePos
		}
		testResults[tgID].TestResults = append(testResults[tgID].TestResults, status)
		if !status.Passed {
			if !status.Skipped {
				testResults[tgID].FailureIndicator = "failed"
				tmplData.NumOfTestFailed++
			} else {
				testResults[tgID].SkippedIndicator = "skipped"
				tmplData.NumOfTestSkipped++
			}
		} else {
			tmplData.NumOfTestPassed++
		}
		tgCounter++
		if tgCounter == tmplData.numOfTestsPerGroup {
			tgCounter = 0
			tgID++
		}
	}
	tmplData.NumOfTests = tmplData.NumOfTestPassed + tmplData.NumOfTestFailed + tmplData.NumOfTestSkipped
	tmplData.TestDuration = elapsedTestTime.Round(time.Millisecond)
	td := time.Now()
	tmplData.TestExecutionDate = fmt.Sprintf("%s %d, %d %02d:%02d:%02d",
		td.Month(), td.Day(), td.Year(), td.Hour(), td.Minute(), td.Second())

	tmplData.TestResults = testResults
	testResultsJson, err := json.Marshal(testResults)

	if err != nil {
		return err
	}

	jsTemplateData.TestResultsJson = string(testResultsJson)

	if err := writeJsFile(jsTemplateData, jsTpl); err != nil {
		return err
	}

	hash, err := calculateFileHash("index.js")

	if err != nil {
		return err
	}

	tmplData.JsIntegrity = htmlTemplate.HTMLAttr(fmt.Sprintf(`integrity="sha256-%s"`, hash))

	hash, err = calculateFileHash("style.css")

	if err != nil {
		return err
	}

	tmplData.CssIntegrity = htmlTemplate.HTMLAttr(fmt.Sprintf(`integrity="sha256-%s"`, hash))

	if err := writeHtmlFile(tmplData, tpl); err != nil {
		return err
	}

	return nil
}

func calculateFileHash(fileName string) (hash string, e error) {
	f, err := os.Open(fileName)

	defer func() {
		if err := f.Close(); err != nil {
			e = err
		}
	}()

	if err != nil {
		e = err
	}

	read, _ := io.ReadAll(f)
	sum256 := sha256.Sum256(read)
	hash = base64.StdEncoding.EncodeToString(sum256[:])
	return hash, e
}

func writeHtmlFile(tmplData *templateData, template *htmlTemplate.Template) (e error) {
	testReportHTMLTemplateFile, _ := os.Create(tmplData.OutputFilename)
	reportFileWriter := bufio.NewWriter(testReportHTMLTemplateFile)
	defer func() {
		if err := reportFileWriter.Flush(); err != nil {
			e = err
		}
		if err := testReportHTMLTemplateFile.Close(); err != nil {
			e = err
		}
	}()

	if err := template.Execute(reportFileWriter, tmplData); err != nil {
		e = err
	}

	return e
}

func writeCssFile(cssTemplateData *cssTemplateData, cssTemplate *textTemplate.Template) (e error) {
	cssFile, _ := os.Create("style.css")
	cssWriter := bufio.NewWriter(cssFile)
	defer func() {
		if err := cssWriter.Flush(); err != nil {
			e = err
		}

		if err := cssFile.Close(); err != nil {
			e = err
		}
	}()

	if err := cssTemplate.Execute(cssWriter, cssTemplateData); err != nil {
		e = err
	}
	return e
}

func writeJsFile(jsTemplateData *jsTemplateData, jsTemplate *textTemplate.Template) (e error) {
	jsFile, _ := os.Create("index.js")
	jsWriter := bufio.NewWriter(jsFile)
	defer func() {
		if err := jsWriter.Flush(); err != nil {
			e = err
		}

		if err := jsFile.Close(); err != nil {
			e = err
		}
	}()

	if err := jsTemplate.Execute(jsWriter, jsTemplateData); err != nil {
		e = err
	}
	return e
}

func parseSizeFlag(cssTemplate *cssTemplateData, flags *cmdFlags) error {
	flags.sizeFlag = strings.ToLower(flags.sizeFlag)
	if !strings.Contains(flags.sizeFlag, "x") {
		val, err := strconv.Atoi(flags.sizeFlag)
		if err != nil {
			return err
		}
		cssTemplate.TestResultGroupIndicatorWidth = fmt.Sprintf("%dpx", val)
		cssTemplate.TestResultGroupIndicatorHeight = fmt.Sprintf("%dpx", val)
		return nil
	}
	if strings.Count(flags.sizeFlag, "x") > 1 {
		return errors.New(`malformed size value; only one x is allowed if specifying with and height`)
	}
	a := strings.Split(flags.sizeFlag, "x")
	valW, err := strconv.Atoi(a[0])
	if err != nil {
		return err
	}
	cssTemplate.TestResultGroupIndicatorWidth = fmt.Sprintf("%dpx", valW)
	valH, err := strconv.Atoi(a[1])
	if err != nil {
		return err
	}
	cssTemplate.TestResultGroupIndicatorHeight = fmt.Sprintf("%dpx", valH)
	return nil
}

func checkIfStdinIsPiped() error {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return err
	}
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		return nil
	}
	return errors.New("ERROR: missing ≪ stdin ≫ pipe")
}
