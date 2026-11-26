{{/* Upstream fixes sslmode to verify-ca. Require hostname verification too. */}}
{{- define "gitlab.psql.ssl.config" -}}
{{- if .Values.global.psql.ssl }}
sslmode: verify-full
sslrootcert: '/etc/gitlab/postgres/ssl/server-ca.pem'
sslcert: '/etc/gitlab/postgres/ssl/client-certificate.pem'
sslkey: '/etc/gitlab/postgres/ssl/client-key.pem'
{{- end -}}
{{- end -}}
