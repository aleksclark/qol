FROM node:24.15.0-alpine AS build
WORKDIR /app
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web .
ARG VITE_API_ORIGIN=http://qol-api:8080
ARG VITE_WEBTRANSPORT_ORIGIN=https://qol-api:4443
RUN npm run build

FROM nginx:1.29-alpine
COPY --from=build /app/dist /usr/share/nginx/html
