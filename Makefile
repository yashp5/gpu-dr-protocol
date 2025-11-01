# Makefile for grene protocol.
# You should be able to build, test, and deploy everything from here.
# Resist complexity, outside tools, and binary dependencies. See dependencies.md for more on this.
# Build philosophy:
# - the #1 most important thing for this whole project is for `make` to "just work". the only
# acceptable process for a dev getting started is:
#   - install make (which should already be installed on most systems)
# 	- git clone https://github.com/grene-labs/grene
# 	- cd grene
# 	- make
# That's it.
#
# Caveats:
# 	Make is extremely opaque and frankly kind of sucks.
#   Anything that's even slightly complicated is usually better farmed out to it's own script you just call.
#   Unfortunately, every other build system seems worse somehow.

export GRENE_LOG=PRETTY # set the log format to pretty, can be changed to JSON or other formats
export GOPRIVATE="github.com/grene-labs/*" # set the private go module path so "go get" and "go mod tidy" work
export POSTGRES_USER=postgres
export POSTGRES_PASSWORD=postgres
export POSTGRES_DB=postgres

# default target: run grene protocol locally with an embedded database.
run: build
	# ----- make run -----
	# run the application
	./grene


# ---- BEGIN DEPENDENCIES & BUILD PROCESS -----
build: godeps gensqlc
	# ----- make build -----
	# build the application
	go build -trimpath -o grene ./cmd/server

# generate Go code from SQL queries using sqlc,
# then copy the schema over to dbutil for embedding into the binary so we can bootstrap the embedded database.
# see sqlc.json for configuration.
gensqlc: godeps
	# ----- make gensqlc -----
	@rm -vf dbutil/*.sql generated/sqlc/*.go
	# ----- create go files in generated/sqlc/ -----
	go tool sqlc generate
	# --- make gensqlc: end ---
	# copy the schema files to dbutil so we can embed them into the binary and use them to
	# spin up the embedded database.
	#
	./copysql.bash . dbutil

# download go dependencies and tools.
godeps: check-go
	# ----- make godeps -----
	# install dependencies and tools via go get / go mod tidy
	go mod tidy -e
	go generate ./...
	# --- make godeps: end ---

lint: godeps gensqlc
	# ----- make lint -----
	# check code quality via staticcheck, go vet, and sqlc: see lint.bash for more details
	/usr/bin/time -p ./lint.bash
	# --- make lint: end ---


# ---- TESTS & BENCHMARKS ------
# run all the tests, including integration and e2e tests.
test: integration-test unit-test e2e-test
	# ----- make test -----
	@echo "All tests passed!" 1>&2
	# ----- make test end -----

unit-test: godeps
	# ----- make unit-test -----
	# run tests that are not disabled via testing.Short() and skip
	go test -vet=off -short -cover -count 1 ./...
	# --- make unit-test: end ---

integration-test: godeps
	# ----- make integration-test -----
	# "long tests" disabled via testing.Short() during unit-test, including
	# - things that touch the network
	# - things that touch the database
	# - just plain slow tests
	go test -vet=off -cover ./...;
	# ----- make integration-test end -----

e2e-test: build
	# ----- make e2e tests -----
	GRENE_RUN_E2E_TESTS=true GRENE_AUTH=BELIEVE_CLIENT go test -count=1 ./end-to-end/...;
	# ----- make e2e tests end -----

benchmark: godeps
	# ----- make benchmark -----
	# run all benchmarks, but not ordinary tests
	go test -vet=off -bench . -run=.+Benchmark.+ -benchmem -count=1 ./...
	# ----- make benchmark end -----

# ---- END TESTS ------


# ---- BEGIN DEPLOYMENT ------

# TODO: deploy scripts

# ---- END DEPLOYMENT ------


# ---- MISCELLANEOUS / OPTIONAL TARGETS ------

profile:
	# dump profiles from the staging instance.
	# use 'go tool pprof $PROFILE_NAME': like
	#	go tool pprof cpu.pprof
	./profileall.bash 'https://protocol.com/api'

# check for jq, the JSON processor, and try and install it if it's not found.
# if you can't, tell the user to install it manually.
check-jq:
	@command -V jq 1>&2 \
	|| (which brew && brew install jq) \
	|| (which apt && sudo apt update && sudo apt install jq) \
	|| { echo "jq not found, please install it: https://jqlang.org/download/"; exit 1; }

# hint to the user that they should install go if they haven't already.
check-go:
	@go version || (echo "go not found in PATH: follow the instructions at https://go.dev/doc/install" && exit 1)

doc:
	# ---- make docs ---
	# serve documentation at localhost:6060
	go tool godoc -http=:6060 &
	@echo "visit http://localhost:6060/pkg/github.com/grene-labs/grene/" to view the documentation
	wait

postgres: godeps
	# ----- make postgres -----
	# install and start postgres
	brew install postgresql && brew services start postgresql

kill-postgres:
	# ----- make kill-postgres -----
	# stop postgres embedded instances
	pkill -f embeddedpg-runtime
