FROM node:22-bookworm-slim

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY src ./src

ENV SANDBOXD_ADDR=0.0.0.0
ENV SANDBOXD_PORT=8092
EXPOSE 8092

CMD ["node", "src/server.mjs"]
