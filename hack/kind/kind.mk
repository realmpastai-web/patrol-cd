# Disposable kind environment targets, included by the root Makefile (`include hack/kind/kind.mk`).
# Only kind clusters named patrol-* and the patrol-registry container are created or deleted;
# kubeconfigs go to .cache/kind/ and ~/.kube/config is never modified.

KIND_DIR := hack/kind
# Images for kind-load; empty = every local patrol-cd/*:dev image.
KIND_IMAGES ?=
# kind-load target: patrol-control, patrol-target or all.
KIND_LOAD_CLUSTER ?= all
# KIND_LOAD_REGISTRY=1 pushes to localhost:5001 instead of `kind load`.
KIND_LOAD_REGISTRY ?=
# Flags for kind-verify, e.g. --lifecycle.
VERIFY_FLAGS ?=
# Flags for kind-restore-drill, e.g. --new-cluster.
RESTORE_DRILL_FLAGS ?=

##@ Kind (disposable local clusters)

.PHONY: kind-up
kind-up: ## Create/start patrol-control, patrol-target and the patrol-registry (idempotent).
	$(KIND_DIR)/up.sh

.PHONY: kind-down
kind-down: ## Delete every patrol-* kind cluster and the patrol-registry container.
	$(KIND_DIR)/down.sh

.PHONY: kind-load
kind-load: ## Load KIND_IMAGES (default: patrol-cd/*:dev) into the clusters; KIND_LOAD_REGISTRY=1 pushes to localhost:5001.
	$(KIND_DIR)/load-image.sh --cluster $(KIND_LOAD_CLUSTER) $(if $(KIND_LOAD_REGISTRY),--registry) $(KIND_IMAGES)

.PHONY: kind-install
kind-install: ## Load patrol-cd/patrol-cd:dev, helm-install Patrol CD into patrol-control, register patrol-target and the e2e fixtures.
	$(KIND_DIR)/install.sh

.PHONY: kind-smoke
kind-smoke: ## Run the fixture smoke checks (cross-cluster API, git over HTTP and SSH, chart repo, OCI, registry images).
	test/e2e/fixtures/smoke.sh --with-registry-image

.PHONY: kind-verify
kind-verify: ## Check the installation against the live clusters (VERIFY_FLAGS=--lifecycle adds rollback, password change, uninstall, re-install).
	$(KIND_DIR)/verify-install.sh $(VERIFY_FLAGS)

.PHONY: kind-restore-drill
kind-restore-drill: ## Back up, lose (the namespace; RESTORE_DRILL_FLAGS=--new-cluster: the cluster), reinstall and restore Patrol CD, then check the restore.
	$(KIND_DIR)/restore-drill.sh $(RESTORE_DRILL_FLAGS)

.PHONY: kind-status
kind-status: ## Show patrol-* clusters, nodes, registry and kubeconfig paths.
	$(KIND_DIR)/status.sh

.PHONY: kind-fixtures
kind-fixtures: ## Build and deploy the e2e fixtures (git, chart repo, OCI artifacts) into patrol-control.
	test/e2e/fixtures/deploy.sh
