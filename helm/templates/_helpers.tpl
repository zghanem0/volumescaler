{{/* Define helper templates */}}
{{- define "volumescaler.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
  {{ .Values.serviceAccount.name }}
{{- else }}
  ""
{{- end }}
{{- end }}