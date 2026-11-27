{{/* The cluster-wide types, plus a namespaced copy per organization with
in-platform builds: only a namespaced ComponentType may allow the
organization's namespaced Workflow, which targets its own workflow plane. */}}
{{- define "aep.componentTypeScopes" -}}
- {kind: ClusterComponentType}
{{- range .Values.buildOrganizations }}
- {kind: ComponentType, namespace: {{ . | quote }}}
{{- end }}
{{- end -}}

{{- define "aep.allowedWorkflows" -}}
{{- $refs := list -}}
{{- range .root.Values.allowedWorkflows }}{{ $refs = append $refs (dict "kind" "ClusterWorkflow" "name" .) }}{{ end -}}
{{- if .scope.namespace }}{{ range .root.Values.namespacedWorkflows }}{{ $refs = append $refs (dict "kind" "Workflow" "name" .) }}{{ end }}{{ end -}}
{{- range $refs }}
  - kind: {{ .kind }}
    name: {{ .name | quote }}
{{- else }} []{{ end -}}
{{- end -}}
