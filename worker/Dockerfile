FROM mcr.microsoft.com/playwright:v1.57.0

WORKDIR /browser_server

COPY ./package.json ./package.json
COPY ./package-lock.json ./package-lock.json

RUN npm ci

COPY ./tsconfig.json ./tsconfig.json
COPY ./src/ ./src/

CMD ["npm", "run", "start"]
