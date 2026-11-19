IMG ?= ghcr.io/thebsdbox/kube-vm-controller:latest
CONTROLLER_GEN = go tool controller-gen

.PHONY: all
all: build

.PHONY: generate
generate: ## Generate deepcopy code.
	$(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: ## Generate the CRD.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: ## Run unit tests.
	go test ./...

.PHONY: build
build: ## Build the manager and the fake daemon into ./bin.
	go build -o bin/manager ./cmd
	go build -o bin/fake-daemon ./cmd/fake-daemon

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: install
install: ## Install the CRD into the current cluster.
	kubectl apply -f config/crd/bases

.PHONY: deploy
deploy: ## Deploy the scheduler and agents.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy:
	kubectl delete -k config/default --ignore-not-found

.PHONY: verify-generated
verify-generated: generate manifests
	git diff --exit-code -- api config/crd
