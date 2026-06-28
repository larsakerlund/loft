# Loft build targets. Go builds directly; the web app and CLI npm packaging shell out.
.PHONY: build daemon cli web test cli-dist scan clean

# Build the daemon and CLI binaries into bin/.
build: daemon cli

daemon:
	go build -o bin/loftd ./cmd/loftd

cli:
	go build -o bin/loft ./cmd/loft

# Build the web root site.
web:
	cd web && pnpm install --frozen-lockfile && pnpm run build

# Run the black-box acceptance suite (needs Docker for Postgres + the LLM emulator).
test:
	cd test && pnpm install && pnpm test

# Cross-compile the CLI for every platform into the npm/ packages, ready to publish loft-cli.
cli-dist:
	./npm/build.sh

# Build both images and scan them for fixable HIGH/CRITICAL vulnerabilities. Run before a release.
# Needs Docker and trivy. DOCKER_HOST is taken from the active docker context so it works on macOS
# (Docker Desktop) as well as Linux.
scan:
	docker build -t loft:scan .
	docker build -t loft-web:scan ./web
	DOCKER_HOST="$$(docker context inspect -f '{{.Endpoints.docker.Host}}')" trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 loft:scan
	DOCKER_HOST="$$(docker context inspect -f '{{.Endpoints.docker.Host}}')" trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 loft-web:scan

clean:
	rm -rf bin npm/loft-cli-*/bin
