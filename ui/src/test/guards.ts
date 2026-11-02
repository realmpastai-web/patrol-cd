/**
 * Test guards, wired up in setup.ts. Problems that would otherwise only print noise, or surface
 * later as a run-level "Unhandled error" in whichever file happened to be running, fail the test
 * that caused them, with a message naming it:
 *
 * - requests no MSW handler matches (the request still fails, as with onUnhandledRequest: 'error');
 * - unhandled promise rejections and uncaught exceptions;
 * - console.error output, also from scripts jsdom runs, except the intentional error-boundary
 *   output in ALLOWED_CONSOLE_ERRORS;
 * - errors jsdom reports, among them what it does not implement (navigation, window.open);
 * - elements left in document.body after cleanup(): UI mounted outside render()/renderApp()
 *   keeps running after its test (queries, watch streams, timers);
 * - real network requests (see blockRealNetwork).
 *
 * Findings made while no test runs (beforeAll, between tests) fail the file in afterAll.
 * Background: docs/verification/runs/2026-10-16-ui-flake.md.
 */
import path from 'node:path';
import { format } from 'node:util';
import { expect } from 'vitest';

interface AllowedConsoleError {
  /** Test file, relative to ui/. */
  file: string;
  /** Full test name as vitest reports it ("describe > test"). */
  test: string;
  /** Matched against the formatted console.error arguments. */
  message: RegExp;
  reason: string;
}

/** What React and React Router log when an error boundary renders a thrown error. */
const ERROR_BOUNDARY_LOG = /The above error occurred in the <\w+> component|React Router caught the following error during render/;

/** Error-boundary output of `error` rendered by a route error boundary. */
function boundaryLog(error: string): RegExp {
  return new RegExp(`(${ERROR_BOUNDARY_LOG.source})[\\s\\S]*${error}|${error}[\\s\\S]*(${ERROR_BOUNDARY_LOG.source})`);
}

/**
 * Intentional console.error output. React (dev build) and React Router log every error an
 * error boundary catches; these tests throw on purpose to exercise the boundaries.
 */
export const ALLOWED_CONSOLE_ERRORS: readonly AllowedConsoleError[] = [
  {
    file: 'src/shell/shell.test.tsx',
    test: 'session gating > shows recovery guidance when the server is unreachable',
    message: boundaryLog('Network error: Failed to fetch'),
    reason: 'userinfo answers HttpResponse.error(); the route error boundary renders "Patrol server unavailable"',
  },
  {
    file: 'src/shell/errors.test.tsx',
    test: 'RouteError > offers sign-in for expired sessions',
    message: boundaryLog('Authentication required'),
    reason: 'a route throws a 401 ApiError; the boundary offers sign-in',
  },
  {
    file: 'src/shell/errors.test.tsx',
    test: 'RouteError > shows technical details and a reload action for unexpected errors',
    message: boundaryLog('render bug'),
    reason: 'a route throws Error("render bug"); the boundary shows technical details',
  },
];

/** Logged by MSW's "error" strategy; reported as an unmocked request instead. */
const MSW_UNHANDLED_REQUEST = '[MSW] Error: intercepted a request without a matching request handler';

interface Finding {
  kind: string;
  detail: string;
}

let currentTest: string | undefined;
let currentFile = '';
const testFindings: Finding[] = [];
const fileFindings: Finding[] = [];

function record(kind: string, detail: string): void {
  (currentTest === undefined ? fileFindings : testFindings).push({ kind, detail });
}

function relativeTestPath(): string {
  const testPath = expect.getState().testPath;
  return testPath ? path.relative(process.cwd(), testPath).split(path.sep).join('/') : '(unknown file)';
}

function describeError(value: unknown): string {
  if (value instanceof Error) {
    const frames = (value.stack ?? '').split('\n').slice(1, 5).join('\n');
    return `${value.name}: ${value.message}${frames ? `\n${frames}` : ''}`;
  }
  return format(value);
}

function firstLines(text: string, count = 6): string {
  const lines = text.split('\n');
  return lines.length > count ? `${lines.slice(0, count).join('\n')}\n…` : text;
}

function isAllowedConsoleError(text: string): boolean {
  return ALLOWED_CONSOLE_ERRORS.some((a) => a.test === currentTest && a.file === currentFile && a.message.test(text));
}

/** MSW onUnhandledRequest callback: records the request and fails it like the "error" strategy. */
export function onUnhandledRequest(request: Request, print: { error: () => void }): void {
  const detail = `${request.method} ${request.url}`;
  record('unmocked request', `${detail} (add an MSW handler for it)`);
  print.error();
  // print.error() only logs: MSW (2.15) lets the request through to the network unless a custom
  // callback throws, and then fails the request with this error.
  throw new Error(`[test-guard] unmocked request: ${detail}`);
}

/**
 * Replaces the fetch that MSW wraps (call before server.listen()). A request MSW does not
 * intercept, made before server.listen() or after server.close() by work that outlived its test,
 * then fails instead of reaching whatever listens on jsdom's origin http://localhost:3000.
 */
export function blockRealNetwork(): void {
  const blocked: typeof fetch = (input, init) => {
    const method = init?.method ?? (input instanceof Request ? input.method : 'GET');
    const url = input instanceof Request ? input.url : input instanceof URL ? input.href : input;
    const message = `[test-guard] blocked a real network request: ${method} ${url}. MSW intercepts requests only between server.listen() and server.close(), so this one comes from work that outlived its test.`;
    if (currentTest === undefined) process.stderr.write(`${message} [${currentFile || relativeTestPath()}]\n`);
    record('real network request', `${method} ${url}`);
    return Promise.reject(new TypeError(message));
  };
  globalThis.fetch = blocked;
}

const onUnhandledRejection = (reason: unknown) => record('unhandled rejection', describeError(reason));
const onUncaughtException = (error: unknown) => record('uncaught exception', describeError(error));

let originalConsoleError: typeof console.error | undefined;

function checkConsoleError(...args: unknown[]): void {
  const text = format(...args);
  if (text.startsWith(MSW_UNHANDLED_REQUEST) || isAllowedConsoleError(text)) return;
  record('console.error', firstLines(text));
}

function guardedConsoleError(...args: unknown[]): void {
  originalConsoleError?.(...args);
  checkConsoleError(...args);
}

const onJsdomError = (error: Error) => record('jsdom error', firstLines(error.stack ?? error.message));

/**
 * The environment's jsdom instance reports here: 'jsdomError' for its own errors, 'error' for
 * console.error in the scripts it runs. vitest creates it before it replaces console, so both go
 * to the console underneath and guardedConsoleError does not see them.
 */
function jsdomConsole(): NodeJS.EventEmitter | undefined {
  return (globalThis as { jsdom?: { virtualConsole: NodeJS.EventEmitter } }).jsdom?.virtualConsole;
}

/** beforeAll: starts watching the process and the console. */
export function startGuards(): void {
  currentFile = relativeTestPath();
  process.on('unhandledRejection', onUnhandledRejection);
  process.on('uncaughtException', onUncaughtException);
  originalConsoleError = console.error;
  console.error = guardedConsoleError;
  jsdomConsole()?.on('jsdomError', onJsdomError);
  jsdomConsole()?.on('error', checkConsoleError);
}

/** afterAll: stops watching and fails the file for findings made while no test ran. */
export function stopGuards(): void {
  process.off('unhandledRejection', onUnhandledRejection);
  process.off('uncaughtException', onUncaughtException);
  jsdomConsole()?.off('jsdomError', onJsdomError);
  jsdomConsole()?.off('error', checkConsoleError);
  if (console.error === guardedConsoleError && originalConsoleError) console.error = originalConsoleError;
  originalConsoleError = undefined;
  const findings = fileFindings.splice(0);
  if (findings.length > 0) throw new Error(failureMessage(`${currentFile} (outside any test)`, findings));
}

/** beforeEach. */
export function beginTest(): void {
  currentTest = expect.getState().currentTestName ?? '(unnamed test)';
  testFindings.length = 0;
}

/** afterEach, after cleanup(): UI still in document.body was mounted outside Testing Library. */
export function checkDocumentAfterCleanup(): void {
  const leftover = [...document.body.children];
  if (leftover.length === 0) return;
  const names = leftover.map((el) => `<${el.tagName.toLowerCase()}${el.id ? ` id="${el.id}"` : ''}>`).join(', ');
  record(
    'DOM left mounted after cleanup()',
    `${names} still in document.body. UI mounted outside render()/renderApp() keeps running after the test (queries, streams, timers); unmount and remove it before the test ends.`,
  );
  document.body.replaceChildren();
}

/** For the guards' own tests: what the running test has recorded, which then does not fail it. */
export function takeTestFindings(): Finding[] {
  return testFindings.splice(0);
}

/** afterEach, last: fails the test that just ran when anything was recorded. */
export function finishTest(): void {
  const name = currentTest ?? '(unnamed test)';
  currentTest = undefined;
  const findings = testFindings.splice(0);
  if (findings.length > 0) throw new Error(failureMessage(`"${name}" in ${currentFile}`, findings));
}

function failureMessage(where: string, findings: readonly Finding[]): string {
  const lines = findings.map((f) => `  - ${f.kind}: ${f.detail.replaceAll('\n', '\n      ')}`);
  return `[test-guard] ${where}:\n${lines.join('\n')}\nSee src/test/guards.ts (allow-listed console.error output: ALLOWED_CONSOLE_ERRORS).`;
}
