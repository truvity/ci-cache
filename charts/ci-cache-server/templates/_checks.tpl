{{/*
The refusals.

Every one of these is a configuration `config.Validate` rejects at start-up,
or one the binary accepts and then gets quietly wrong. Refusing here is not
duplication: the chart catches it before a cluster sees it, and the binary
catches it when somebody runs the server by hand. What renders is what the
image will run.

Order matters. The narrow rule comes before the broad one, so that a caller
who set an endpoint and forgot a region is told about the endpoint rather than
being handed the general sentence about regions.
*/}}
{{- define "ci-cache.checks" -}}

{{- if not .Values.store.bucket -}}
{{- fail "ci-cache: set `store.bucket`. The disk is the fast copy of a bucket; with no bucket there is nothing behind it, and every restart starts from nothing." -}}
{{- end -}}

{{- if and .Values.store.endpoint (not .Values.store.region) -}}
{{- fail "ci-cache: `store.endpoint` is set and `store.region` is empty. A store that is not AWS still has to be told a region, because the SDK signs with one; Cloudflare R2 answers to \"auto\". Left empty the SDK guesses from whatever the pod's environment carries, and the first request fails on a signature, which reads as a credentials problem and is not." -}}
{{- end -}}

{{- if not .Values.store.region -}}
{{- fail "ci-cache: set `store.region`. Inside a pod the SDK's own resolution is whatever the environment happens to carry, which differs between a node with a metadata service and one without. The server refuses to start without it." -}}
{{- end -}}

{{- if and .Values.store.existingSecret .Values.serviceAccount.annotations -}}
{{- fail "ci-cache: `store.existingSecret` and `serviceAccount.annotations` are two identities for one process. Static keys in a Secret and a pod-identity annotation both reach the SDK, and which one wins is a property of the SDK's version rather than of this file. Keep the keys, or keep the annotation." -}}
{{- end -}}

{{- if eq (int .Values.service.data) (int .Values.service.admin) -}}
{{- fail "ci-cache: `service.data` and `service.admin` must differ. The admin port is a boundary, not a path: it carries the wipes, and one port would put them behind the address every CI job is given. The server refuses to start this way." -}}
{{- end -}}

{{- if and (not .Values.persistence.enabled) (not .Values.persistence.ephemeralIsAcceptable) -}}
  {{- if or .Values.frontends.go.build.enabled .Values.frontends.gradle.build.enabled -}}
  {{- fail "ci-cache: a build-cache front-end (`frontends.go.build.enabled` or `frontends.gradle.build.enabled`) with `persistence.enabled: false`. The volume would be an emptyDir, so every restart empties the cache -- and nothing fails visibly: each request still answers, always with a miss, and the builds simply stay as slow as they were. Turn persistence on, or set `persistence.ephemeralIsAcceptable: true` to say a cold cache is intended." -}}
  {{- end -}}
{{- end -}}

{{- if or (lt (int .Values.gc.floor) 0) (gt (int .Values.gc.floor) 90) -}}
{{- fail (printf "ci-cache: `gc.floor` is a percentage of the filesystem left free, between 0 and 90, not %v. A full volume is not a slow cache but a failing writer." .Values.gc.floor) -}}
{{- end -}}

{{- if ge (int .Values.gc.low) (int .Values.gc.high) -}}
{{- fail (printf "ci-cache: `gc.low` (%v) must be below `gc.high` (%v). Eviction starts at high and runs until low; with low at or above high there is no band to evict into and the collector would run forever." .Values.gc.low .Values.gc.high) -}}
{{- end -}}

{{- if gt (int .Values.gc.high) 100 -}}
{{- fail (printf "ci-cache: `gc.high` is a percentage of the budget, at most 100, not %v." .Values.gc.high) -}}
{{- end -}}

{{- if and .Values.frontends.maven.enabled (not .Values.frontends.maven.upstreams) -}}
{{- fail "ci-cache: `frontends.maven.enabled` needs at least one entry in `frontends.maven.upstreams`. Each becomes /maven/<name>; with none the front-end mounts no route at all." -}}
{{- end -}}

{{- range $name, $url := .Values.frontends.maven.upstreams -}}
{{- if not (or (hasPrefix "http://" $url) (hasPrefix "https://" $url)) -}}
{{- fail (printf "ci-cache: `frontends.maven.upstreams[%s]` must be an absolute URL, got %q. A bare host reaches the client as a path." $name $url) -}}
{{- end -}}
{{- end -}}

{{- if and .Values.frontends.gradle.dist.enabled (not .Values.frontends.gradle.dist.allow) -}}
{{- fail "ci-cache: `frontends.gradle.dist.enabled` needs `frontends.gradle.dist.allow`. A distribution proxy with no allow list is an open relay: anything that can reach the data port can make the cache fetch any URL and hand it back from a name the estate trusts." -}}
{{- end -}}

{{- if and .Values.frontends.nix.enabled (not .Values.frontends.nix.upstreams) -}}
{{- fail "ci-cache: `frontends.nix.enabled` needs at least one entry in `frontends.nix.upstreams`. A substituter with nothing behind it answers every path with a miss, and Nix builds everything locally while looking configured." -}}
{{- end -}}

{{- if not .Values.frontends.go.build.enabled -}}
  {{- if not (or .Values.frontends.go.mod.enabled .Values.frontends.maven.enabled .Values.frontends.gradle.build.enabled .Values.frontends.gradle.dist.enabled .Values.frontends.nix.enabled .Values.frontends.npm.enabled .Values.frontends.bazel.enabled) -}}
  {{- fail "ci-cache: every front-end is off. The server would start, pass its probes and serve nothing, which is the one failure a running Deployment cannot show you." -}}
  {{- end -}}
{{- end -}}

{{- end -}}
