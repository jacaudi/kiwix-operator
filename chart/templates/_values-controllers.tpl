{{/*
Build controllers structure from flat values
*/}}
{{- define "kiwix-operator.values.controllers" -}}
{{- $image := printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
controllers:
  main:
    type: deployment
    replicas: {{ .Values.controller.replicas }}
    strategy: {{ .Values.controller.strategy }}
    serviceAccount:
      identifier: main
    containers:
      main:
        image:
          repository: {{ .Values.image.repository }}
          tag: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
          pullPolicy: {{ .Values.image.pullPolicy }}
        args:
          - manager
          - --leader-elect
          - --metrics-bind-address=:8080
          - --health-probe-bind-address=:8081
          - --operator-image={{ $image }}
          - --zimit-image={{ required "zimit.image is required" .Values.zimit.image }}
          - --catalog-url={{ .Values.catalogURL }}
          - --webhook-retries={{ .Values.webhookRetries }}
          - --crawl-egress-allow-cidrs={{ join "," .Values.crawlEgressAllowCIDRs }}
          - --probe-allow-cidrs={{ join "," .Values.probeAllowCIDRs }}
        env:
          POD_NAMESPACE:
            valueFrom:
              fieldRef:
                fieldPath: metadata.namespace
        resources:
          {{- toYaml .Values.resources | nindent 10 }}
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop:
              - ALL
        probes:
          liveness:
            enabled: true
            custom: true
            spec:
              httpGet:
                path: /healthz
                port: 8081
              initialDelaySeconds: 15
              periodSeconds: 20
          readiness:
            enabled: true
            custom: true
            spec:
              httpGet:
                path: /readyz
                port: 8081
              initialDelaySeconds: 5
              periodSeconds: 10
{{- end -}}
