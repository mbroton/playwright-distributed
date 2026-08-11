FROM mcr.microsoft.com/playwright:v1.62.1

WORKDIR /browser_server

COPY ./package.json ./package.json
COPY ./package-lock.json ./package-lock.json

RUN npm ci

COPY ./tsconfig.json ./tsconfig.json
COPY ./src/ ./src/

USER pwuser

CMD ["npm", "run", "start"]
