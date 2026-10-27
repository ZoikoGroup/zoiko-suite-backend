Add-Type -AssemblyName DocumentFormat.OpenXml
$doc = [DocumentFormat.OpenXml.Packaging.WordprocessingDocument]::Open("docs/architecture/ZoikoSuite_Engineering_Verification_Certification_Acceptance_Standard_Detailed_Engineering_Wireframe.docx", $false)
$body = $doc.MainDocumentPart.Document.Body
$text = $body.InnerText
$doc.Close()
$text | Out-File -FilePath "docs/architecture/standard_text.txt" -Encoding UTF8