{{- define "aep.mirror" -}}
{{- $ref := .ref -}}
{{- $mirror := trimSuffix "/" (.root.Values.global.imageRegistry | default "") -}}
{{- $oci := hasPrefix "oci://" $ref -}}
{{- $ref = trimPrefix "oci://" $ref -}}
{{- if and $mirror (not (hasPrefix (printf "%s/" $mirror) $ref)) -}}
{{- $first := first (splitList "/" $ref) -}}
{{- if not (contains "/" $ref) -}}{{- $ref = printf "docker.io/library/%s" $ref -}}
{{- else if and (not (contains "." $first)) (not (contains ":" $first)) (ne $first "localhost") -}}{{- $ref = printf "docker.io/%s" $ref -}}{{- end -}}
{{- $parts := splitList "/" $ref -}}
{{- $host := first $parts | replace ":" "_" -}}
{{- if or (eq $host "index.docker.io") (eq $host "registry-1.docker.io") -}}{{- $host = "docker.io" -}}{{- end -}}
{{- $path := join "/" (rest $parts) -}}
{{- if and (eq $host "docker.io") (not (contains "/" $path)) -}}{{- $path = printf "library/%s" $path -}}{{- end -}}
{{- $ref = printf "%s/%s/%s" $mirror $host $path -}}
{{- end -}}
{{- if $oci -}}oci://{{- end -}}{{- $ref -}}
{{- end -}}
