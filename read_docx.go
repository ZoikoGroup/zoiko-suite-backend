package main

import (
	"fmt"
	"log"
	"os"

	"github.com/unidoc/unioffice/document"
)

func main() {
	doc, err := document.Open("docs/architecture/ZoikoSuite_Engineering_Verification_Certification_Acceptance_Standard_Detailed_Engineering_Wireframe.docx")
	if err != nil {
		log.Fatal(err)
	}
	defer doc.Close()

	for _, para := range doc.Paragraphs() {
		fmt.Println(para.Text())
	}
}