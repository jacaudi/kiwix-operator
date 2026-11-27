{{/*
Return the full name for the chart
*/}}
{{- define "kiwix-operator.fullname" -}}
{{- include "bjw-s.common.lib.chart.names.fullname" . -}}
{{- end -}}

{{/*
Return the chart name
*/}}
{{- define "kiwix-operator.name" -}}
{{- include "bjw-s.common.lib.chart.names.name" . -}}
{{- end -}}
