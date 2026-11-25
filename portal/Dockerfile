# Build with the repository root as context.
FROM node:24-bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ libsqlite3-dev ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY portal/ ./
ENV NODE_OPTIONS="--max-old-space-size=8192 --no-node-snapshot"
RUN npx --yes @yarnpkg/cli-dist@4.13.0 install --immutable
RUN npx --yes @yarnpkg/cli-dist@4.13.0 tsc
RUN npx --yes @yarnpkg/cli-dist@4.13.0 build:backend

FROM node:24-bookworm-slim AS dependencies
RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ libsqlite3-dev ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /app/package.json /app/yarn.lock /app/.yarnrc.yml /app/backstage.json ./
COPY --from=build /app/packages/backend/dist/skeleton.tar.gz ./
RUN tar xzf skeleton.tar.gz && rm skeleton.tar.gz
ENV NODE_OPTIONS="--no-node-snapshot"
RUN npx --yes @yarnpkg/cli-dist@4.13.0 workspaces focus --all --production
COPY --from=build /app/packages/backend/dist/bundle.tar.gz ./
RUN tar xzf bundle.tar.gz && rm bundle.tar.gz

FROM node:24-bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libsqlite3-0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ENV NODE_ENV=production NODE_OPTIONS="--no-node-snapshot"
COPY --from=dependencies --chown=node:node /app ./
COPY --from=build --chown=node:node /app/app-config.yaml /app/app-config.production.yaml ./
USER node
EXPOSE 7007
CMD ["node", "packages/backend", "--config", "app-config.yaml", "--config", "app-config.production.yaml"]
