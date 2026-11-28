{{- define "aep.platformNamespaces" -}}
{{- $names := list "kube-system" "cozy-*" "openchoreo-*" "kyverno" "aep-system" "aep-kafka" "cert-manager*" -}}
{{- range .Values.platformNamespaces -}}
{{- if or (hasPrefix "dp-" .) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .)) (gt (len .) 63) -}}
{{- fail "platformNamespaces must contain exact namespace names outside dp-*" -}}
{{- end -}}
{{- $names = append $names . -}}
{{- end -}}
{{- toYaml (uniq $names) -}}
{{- end -}}
