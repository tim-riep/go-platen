{{ range . }}
================================================================================
{{ .Name }} {{ .Version }}
License: {{ .LicenseName }}
License file: {{ .LicenseURL }}
Source: https://{{ .Name }} (version {{ .Version }})
--------------------------------------------------------------------------------
{{ .LicenseText }}
{{ end }}
