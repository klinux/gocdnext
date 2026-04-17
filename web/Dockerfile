FROM node:20-alpine AS build
WORKDIR /src
COPY package.json ./
RUN npm install
COPY . .
RUN npm run build

FROM node:20-alpine
WORKDIR /app
COPY --from=build /src/.next ./.next
COPY --from=build /src/node_modules ./node_modules
COPY --from=build /src/package.json ./package.json
COPY --from=build /src/next.config.ts ./next.config.ts
EXPOSE 3000
CMD ["npm", "start"]
