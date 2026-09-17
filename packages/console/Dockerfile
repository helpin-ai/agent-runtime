FROM node:24.21.0-bookworm-slim@sha256:2fe369e969550cde8e867afc3fe370b260140cab4a23d467074295b42163d553 AS build

WORKDIR /app
RUN corepack enable && corepack prepare pnpm@10.32.1 --activate

COPY packages/console/package.json packages/console/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY packages/console/ ./
RUN pnpm run build

FROM node:24.21.0-bookworm-slim@sha256:2fe369e969550cde8e867afc3fe370b260140cab4a23d467074295b42163d553 AS runtime
COPY LICENSE NOTICE /usr/share/licenses/agent-runtime/

ENV NODE_ENV=production \
    HOST=0.0.0.0 \
    PORT=3000

WORKDIR /app
COPY --from=build --chown=node:node /app/.output ./.output

USER node
EXPOSE 3000

CMD ["node", ".output/server/index.mjs"]
