package main

import (
	_ "embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

//go:embed setup.html
var setupHTML string

var setupPage = template.Must(template.New("setup").Parse(setupHTML))

// serveSetup shows how to add the browser shortcut. Its OpenSearch link also lets browsers
// offer pr-jump as a search engine.
func (a *app) serveSetup(w http.ResponseWriter, r *http.Request) {
	var prs int
	if err := a.db.QueryRow("select count(*) from prs").Scan(&prs); err != nil {
		a.log.Printf("counting PRs: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := setupPage.Execute(w, struct {
		Base, Owners, PRs string
	}{"http://" + r.Host, strings.Join(a.owners, ", "), thousands(prs)})
	if err != nil {
		a.log.Printf("setup page: %v", err)
	}
}

// serveOpenSearch describes pr-jump as a search engine, in the OpenSearch format browsers read.
func (a *app) serveOpenSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>pr-jump</ShortName>
  <Description>Go straight to a pull request by its number</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Url type="text/html" method="get" template="http://` + xmlText(r.Host) + `/?q={searchTerms}"/>
</OpenSearchDescription>
`))
}

// thousands formats 27287 as "27,287", and 0 as "" so the page can say the index is building.
func thousands(n int) string {
	if n == 0 {
		return ""
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
