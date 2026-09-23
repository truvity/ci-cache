{{- define "ci-cache.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ci-cache.fullname" -}}
{{- $name := .Chart.Name -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "ci-cache.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "ci-cache.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "ci-cache.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ci-cache.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "ci-cache.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "ci-cache.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ci-cache.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The claim the disk tier mounts, whether this chart makes it or not. */}}
{{- define "ci-cache.claimName" -}}
{{- .Values.persistence.existingClaim | default (printf "%s-data" (include "ci-cache.fullname" .)) -}}
{{- end -}}

{{/*
Seconds from a Go duration, so that a number derived from one is a number and
not a string comparison that passes silently. Helm has no duration type.
*/}}
{{- define "ci-cache.seconds" -}}
{{- $d := . | toString -}}
{{- if hasSuffix "ms" $d -}}
{{- div (trimSuffix "ms" $d | float64 | int) 1000 -}}
{{- else if hasSuffix "h" $d -}}
{{- mul (trimSuffix "h" $d | float64 | int) 3600 -}}
{{- else if hasSuffix "m" $d -}}
{{- mul (trimSuffix "m" $d | float64 | int) 60 -}}
{{- else -}}
{{- trimSuffix "s" $d | float64 | int -}}
{{- end -}}
{{- end -}}

{{/*
A map rendered as the one string a single environment variable can carry:
key=value pairs, comma separated. Go templates walk a map in key order, so the
same values file renders the same string and a golden diff means a change.
*/}}
{{- define "ci-cache.pairs" -}}
{{- $out := list -}}
{{- range $k, $v := . -}}
{{- $out = append $out (printf "%s=%v" $k $v) -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{/*
The configuration, as the environment.

Every name below is the upper snake case of its path in values.yaml, which is
its path in config.Config: `frontends.go.mod.listTTL` is
CI_CACHE_FRONTENDS_GO_MOD_LIST_TTL. Adding a field to the struct and not to
this list is the one way the chart and the binary can come to describe
different things, which is what `just docs` checks.

Numbers and booleans are rendered whatever they are, because their zero is a
choice somebody may have made. Strings are rendered only when non-empty: an
empty CI_CACHE_STORE_ENDPOINT is not "use the AWS default", it is an endpoint
whose host is the empty string, and the SDK takes it literally.
*/}}
{{- define "ci-cache.env" -}}
- name: CI_CACHE_STORE_BUCKET
  value: {{ .Values.store.bucket | quote }}
- name: CI_CACHE_STORE_REGION
  value: {{ .Values.store.region | quote }}
{{- with .Values.store.endpoint }}
- name: CI_CACHE_STORE_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_STORE_PATH_STYLE
  value: {{ .Values.store.pathStyle | quote }}
{{- with .Values.store.keyPrefix }}
- name: CI_CACHE_STORE_KEY_PREFIX
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_GO_BUILD_ENABLED
  value: {{ .Values.frontends.go.build.enabled | quote }}
{{- with .Values.frontends.go.build.legacyPrefix }}
- name: CI_CACHE_FRONTENDS_GO_BUILD_LEGACY_PREFIX
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_GO_MOD_ENABLED
  value: {{ .Values.frontends.go.mod.enabled | quote }}
{{- with .Values.frontends.go.mod.upstream }}
- name: CI_CACHE_FRONTENDS_GO_MOD_UPSTREAM
  value: {{ . | quote }}
{{- end }}
{{- with .Values.frontends.go.mod.sumdb }}
- name: CI_CACHE_FRONTENDS_GO_MOD_SUMDB
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_GO_MOD_LIST_TTL
  value: {{ .Values.frontends.go.mod.listTTL | quote }}
{{- with .Values.frontends.go.mod.legacyPrefix }}
- name: CI_CACHE_FRONTENDS_GO_MOD_LEGACY_PREFIX
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_MAVEN_ENABLED
  value: {{ .Values.frontends.maven.enabled | quote }}
{{- with .Values.frontends.maven.upstreams }}
- name: CI_CACHE_FRONTENDS_MAVEN_UPSTREAMS
  value: {{ include "ci-cache.pairs" . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_MAVEN_METADATA_TTL
  value: {{ .Values.frontends.maven.metadataTTL | quote }}
- name: CI_CACHE_FRONTENDS_MAVEN_NEGATIVE_TTL
  value: {{ .Values.frontends.maven.negativeTTL | quote }}
- name: CI_CACHE_FRONTENDS_GRADLE_BUILD_ENABLED
  value: {{ .Values.frontends.gradle.build.enabled | quote }}
- name: CI_CACHE_FRONTENDS_GRADLE_BUILD_READ_ONLY
  value: {{ .Values.frontends.gradle.build.readOnly | quote }}
- name: CI_CACHE_FRONTENDS_GRADLE_BUILD_MAX_ENTRY
  value: {{ .Values.frontends.gradle.build.maxEntry | int64 | quote }}
- name: CI_CACHE_FRONTENDS_GRADLE_DIST_ENABLED
  value: {{ .Values.frontends.gradle.dist.enabled | quote }}
{{- with .Values.frontends.gradle.dist.allow }}
- name: CI_CACHE_FRONTENDS_GRADLE_DIST_ALLOW
  value: {{ join "," . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_NIX_ENABLED
  value: {{ .Values.frontends.nix.enabled | quote }}
{{- with .Values.frontends.nix.upstreams }}
- name: CI_CACHE_FRONTENDS_NIX_UPSTREAMS
  value: {{ join "," . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_NIX_PRIORITY
  value: {{ .Values.frontends.nix.priority | int | quote }}
- name: CI_CACHE_FRONTENDS_NIX_NEGATIVE_TTL
  value: {{ .Values.frontends.nix.negativeTTL | quote }}
- name: CI_CACHE_FRONTENDS_NPM_ENABLED
  value: {{ .Values.frontends.npm.enabled | quote }}
{{- with .Values.frontends.npm.upstream }}
- name: CI_CACHE_FRONTENDS_NPM_UPSTREAM
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_FRONTENDS_NPM_METADATA_TTL
  value: {{ .Values.frontends.npm.metadataTTL | quote }}
- name: CI_CACHE_FRONTENDS_BAZEL_ENABLED
  value: {{ .Values.frontends.bazel.enabled | quote }}
- name: CI_CACHE_PERSISTENCE_DIR
  value: {{ .Values.persistence.dir | quote }}
{{- /*
Told to the binary as well as to the volume. Without persistence this is an
emptyDir mounted at a path the binary's own heuristic would not recognise as
ephemeral, so the chart is the only thing that knows -- and the binary's
refusal is the second gate, not the first.
*/}}
- name: CI_CACHE_PERSISTENCE_EPHEMERAL_IS_ACCEPTABLE
  value: {{ or .Values.persistence.ephemeralIsAcceptable (not .Values.persistence.enabled) | quote }}
- name: CI_CACHE_GC_FLOOR
  value: {{ .Values.gc.floor | int | quote }}
- name: CI_CACHE_GC_HIGH
  value: {{ .Values.gc.high | int | quote }}
- name: CI_CACHE_GC_LOW
  value: {{ .Values.gc.low | int | quote }}
- name: CI_CACHE_GC_BUDGET_BYTES
  value: {{ .Values.gc.budgetBytes | int64 | quote }}
{{- with .Values.gc.budgets }}
- name: CI_CACHE_GC_BUDGETS
  value: {{ include "ci-cache.pairs" . | quote }}
{{- end }}
- name: CI_CACHE_UPLOAD_CONCURRENCY
  value: {{ .Values.upload.concurrency | int | quote }}
- name: CI_CACHE_UPLOAD_QUEUE
  value: {{ .Values.upload.queue | int | quote }}
- name: CI_CACHE_UPLOAD_QUEUE_BYTES
  value: {{ .Values.upload.queueBytes | int64 | quote }}
- name: CI_CACHE_UPLOAD_MIN_SIZE
  value: {{ .Values.upload.minSize | int64 | quote }}
- name: CI_CACHE_SERVICE_DATA
  value: {{ .Values.service.data | int | quote }}
- name: CI_CACHE_SERVICE_ADMIN
  value: {{ .Values.service.admin | int | quote }}
- name: CI_CACHE_SERVER_CONCURRENCY
  value: {{ .Values.server.concurrency | int | quote }}
- name: CI_CACHE_SERVER_DRAIN_TIMEOUT
  value: {{ .Values.server.drainTimeout | quote }}
{{- with .Values.telemetry.otlpEndpoint }}
- name: CI_CACHE_TELEMETRY_OTLP_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: CI_CACHE_TELEMETRY_TRACE_RATIO
  value: {{ .Values.telemetry.traceRatio | quote }}
- name: CI_CACHE_LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
{{- end -}}
