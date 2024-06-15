package printer

import (
	"bytes"
	"html/template"
	"io"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
)

type Client interface {
	PrintTextTable(data *[]table.Row) error
	PrintHtmlTable(title string, header []string, data [][]string) ([]byte, error)
}

type printerConfig struct {
	Writer       io.Writer
	Index        *bool
	Header       *table.Row
	ColumnConfig *[]table.ColumnConfig
	Sort         *[]table.SortBy
}

func NewPrinter(writer io.Writer, index *bool, header *table.Row, sort *[]table.SortBy, columnConfig *[]table.ColumnConfig) Client {
	c := &printerConfig{
		Writer:       writer,
		Index:        index,
		Header:       header,
		ColumnConfig: columnConfig,
		Sort:         sort,
	}

	return &client{Printer: c}
}

type client struct {
	Printer *printerConfig
}

func (p *client) PrintTextTable(data *[]table.Row) error {
	t := table.NewWriter()
	t.SetOutputMirror(p.Printer.Writer)
	t.SetAutoIndex(*p.Printer.Index)
	t.AppendHeader(*p.Printer.Header)
	// t.AppendFooter(*p.Footer)
	t.AppendRows(*data)
	t.Style().Options.SeparateRows = true
	t.SortBy(*p.Printer.Sort)
	t.SetColumnConfigs(*p.Printer.ColumnConfig)
	t.Render()

	return nil
}

func (p *client) PrintHtmlTable(title string, header []string, data [][]string) ([]byte, error) {
	funcMap := template.FuncMap{
		"nl2br": func(text string) template.HTML {
			return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(text), "\n", "<br>"))
		},
	}

	tableTemplate, err := template.New("").Funcs(funcMap).Parse(`<html>
<head>
<title>{{ .Title }}</title>
<style type="text/css">
table {
  font-family: verdana,arial,sans-serif;
  font-size:11px;
  color:#333333;
  border-width: 2px;
  border-color: #595959;
  border-collapse: collapse;
}
table th {
  background-color:#99ff99;
  border-width: 2px;
  padding: 8px;
  border-style: solid;
  border-color: #595959;
}
table tr {
  background-color:##737373;
}
table td {
  border-width: 2px;
  padding: 8px;
  border-style: solid;
  border-color: #595959;
}
</style>
</head>
<body>
<table>
	<thead>
		<tr>
			{{range .Header}}<th>{{ nl2br . }}</th>{{end}}
		</tr>
	</thead>
	<tbody>
		{{range .Data}}
		<tr>
			{{range .}}<td>{{ nl2br . }}</td>{{end}}
		</tr>
		{{end}}
	</tbody>
</table>
</body>
</html>`)

	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	params := htmlTemplateParams{Title: title, Header: header, Data: data}
	if err = tableTemplate.Execute(&body, params); err != nil {
		return nil, err
	}

	return body.Bytes(), nil
}

type htmlTemplateParams struct {
	Title  string
	Header []string
	Data   [][]string
}

// func Footer(footer table.Row) *table.Row {
// 	if len(footer) > 1 {
// 		return &footer
// 	} else {
// 		return nil
// 	}
// }
