import { loadConfig } from './config.js';
import { BrowserWorker } from './worker.js';

const worker = new BrowserWorker(loadConfig());

process.on('SIGTERM', () => void worker.requestDrain('SIGTERM', true));
process.on('SIGINT', () => void worker.requestDrain('SIGINT', true));
process.on('unhandledRejection', error => void worker.fatal(error));
process.on('uncaughtException', error => void worker.fatal(error));

await worker.start();
