# SPDX-License-Identifier: MPL-2.0

TEST?=./...
TESTARGS?=
ACCTEST?=-run='^TestAcc'
ACCTEST_TIMEOUT?=180m

default: vet build

build:
	go build -o terraform-provider-llm .

test:
	go test $(TEST) -timeout=120s $(TESTARGS)

testacc:
	TF_ACC=1 go test $(TEST) $(ACCTEST) -v -timeout $(ACCTEST_TIMEOUT) $(TESTARGS)

generate:
	go generate ./...

fmt:
	gofmt -s -w .
	terraform fmt -recursive examples/

fmt-check:
	if [ -n "$$(gofmt -l .)" ]; then gofmt -l .; exit 1; fi
	terraform fmt -check -recursive examples/

vet:
	go vet $(TEST)

.PHONY: default build test testacc generate fmt fmt-check vet
