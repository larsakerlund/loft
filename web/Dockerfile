# Web image for Loft: build the root site, then serve it, deployed sites, uploads, and the
# placeholder pages over plain static nginx. Standalone it is also the ingress (proxying /api to
# loftd); a real deployment fronts it with its own authenticating proxy. nginx renders nginx.conf
# from /templates at start, substituting LOFT_DOMAIN (the apex host; defaults to localhost for dev).
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
ENV LOFT_DOMAIN=localhost
COPY nginx.conf /etc/nginx/templates/default.conf.template
COPY sys/ /usr/share/nginx/__loft-sys/
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 8080
