package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
)

var htmlTemplate []byte
var jsCode []byte
var jsTemplate []byte
var cssTemplate []byte

func init() {
	htmlTemplate, _ = os.ReadFile("../templates/test_report.html.template")
	jsCode, _ = os.ReadFile("../templates/test_report.js")
	jsTemplate, _ = os.ReadFile("../templates/test_report.js.template")
	cssTemplate, _ = os.ReadFile("../templates/style.css.template")
}

func main() {
	outputFile1, _ := os.Create("../embedded_assets.go")
	writer := bufio.NewWriter(outputFile1)
	defer func() {
		if err := writer.Flush(); err != nil {
			panic(err)
		}
		if err := outputFile1.Close(); err != nil {
			panic(err)
		}
	}()
	dst := make([]byte, hex.EncodedLen(len(htmlTemplate)))
	hex.Encode(dst, htmlTemplate)
	_, _ = writer.WriteString(fmt.Sprintf("package main\n\nvar testReportHTMLTemplate = `%s`", string(dst)))
	dst = make([]byte, hex.EncodedLen(len(jsCode)))
	hex.Encode(dst, jsCode)
	_, _ = writer.WriteString(fmt.Sprintf("\n\nvar testReportJsCode = `%s`", string(dst)))
	dst = make([]byte, hex.EncodedLen(len(jsTemplate)))
	hex.Encode(dst, jsTemplate)
	_, _ = writer.WriteString(fmt.Sprintf("\n\nvar testReportJsTemplate = `%s`", string(dst)))
	dst = make([]byte, hex.EncodedLen(len(cssTemplate)))
	hex.Encode(dst, cssTemplate)
	_, _ = writer.WriteString(fmt.Sprintf("\n\nvar testReportCssTemplate = `%s`\n", string(dst)))
}
