# Local-dev web image: build the root site, then serve it (and proxy /api to loftd) with plain
# nginx. This is the simple, generic ingress for running Loft locally. A real deployment fronts
# loftd with its own authenticating proxy instead of this image.
#
# Build context is web/: docker build -t loft-web web/

FROM node:24-alpine AS build
WORKDIR /app
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY . .
RUN pnpm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 8080
