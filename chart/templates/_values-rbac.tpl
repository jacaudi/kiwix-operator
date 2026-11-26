{{/*
RBAC configuration for kiwix-operator
AUTO-GENERATED from config/rbac/role.yaml - DO NOT EDIT MANUALLY
Run 'task generate-helm-rbac' to regenerate after updating kubebuilder markers.
*/}}
{{- define "kiwix-operator.values.rbac" -}}
{{- if .Values.rbac.enabled }}
rbac:
  roles:
    main:
      enabled: true
      type: ClusterRole
      rules:
        - apiGroups:
            - ""
          resources:
            - configmaps
          verbs:
            - create
            - get
            - list
            - watch
        - apiGroups:
            - ""
          resources:
            - persistentvolumeclaims
            - services
          verbs:
            - create
            - get
            - list
            - update
            - watch
        - apiGroups:
            - ""
          resources:
            - pods
          verbs:
            - get
            - list
            - watch
        - apiGroups:
            - ""
          resources:
            - secrets
          verbs:
            - create
            - delete
            - get
            - list
            - update
            - watch
        - apiGroups:
            - ""
            - events.k8s.io
          resources:
            - events
          verbs:
            - create
            - patch
        - apiGroups:
            - apps
          resources:
            - deployments
          verbs:
            - create
            - delete
            - get
            - list
            - update
            - watch
        - apiGroups:
            - batch
          resources:
            - jobs
          verbs:
            - create
            - get
            - list
            - watch
        - apiGroups:
            - batch
          resources:
            - jobs/finalizers
          verbs:
            - update
        - apiGroups:
            - coordination.k8s.io
          resources:
            - leases
          verbs:
            - create
            - delete
            - get
            - list
            - patch
            - update
            - watch
        - apiGroups:
            - gateway.networking.k8s.io
          resources:
            - httproutes
          verbs:
            - create
            - delete
            - get
            - list
            - update
            - watch
        - apiGroups:
            - kiwix.jacaudi.dev
          resources:
            - kiwixlibraries
          verbs:
            - get
            - list
            - watch
        - apiGroups:
            - kiwix.jacaudi.dev
          resources:
            - kiwixlibraries/finalizers
            - zimarchives/finalizers
          verbs:
            - update
        - apiGroups:
            - kiwix.jacaudi.dev
          resources:
            - kiwixlibraries/status
            - zimarchives/status
          verbs:
            - get
            - update
        - apiGroups:
            - kiwix.jacaudi.dev
          resources:
            - zimarchives
          verbs:
            - get
            - list
            - update
            - watch
        - apiGroups:
            - networking.k8s.io
          resources:
            - networkpolicies
          verbs:
            - create
            - get
            - list
            - update
            - watch
  bindings:
    main:
      enabled: true
      type: ClusterRoleBinding
      roleRef:
        identifier: main
      subjects:
        - identifier: main
{{- end }}
{{- end -}}
