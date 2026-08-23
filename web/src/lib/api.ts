// The console's API client.
//
// Every call is same-origin and carries the session cookie, so there is no
// token to store in the browser and nothing for a script to steal. A 401 means
// the session ended, and is surfaced as a distinct error type so the app can
// redirect to sign-in rather than showing a generic failure.

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }

  /** The session has ended or was never established. */
  get isUnauthenticated(): boolean {
    return this.status === 401;
  }

  /** Signed in, but not permitted to do this. */
  get isForbidden(): boolean {
    return this.status === 403;
  }
}

type RequestOptions = {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
};

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, signal } = options;

  const response = await fetch(path, {
    method,
    signal,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    // Cookies are same-origin here, but stating it makes the intent explicit
    // and survives someone later pointing the app at another origin.
    credentials: "same-origin",
  });

  if (response.status === 204) {
    return undefined as T;
  }

  const text = await response.text();
  let payload: unknown;
  try {
    payload = text ? JSON.parse(text) : {};
  } catch {
    // A non-JSON body from an API route means something upstream failed —
    // a proxy error page, typically. Reporting the status is more useful than
    // reporting a parse error.
    throw new ApiError(response.status, `The server returned an unexpected response (${response.status}).`);
  }

  if (!response.ok) {
    const message =
      typeof payload === "object" && payload !== null && "error" in payload
        ? String((payload as { error: unknown }).error)
        : `Request failed (${response.status}).`;
    throw new ApiError(response.status, message);
  }

  return payload as T;
}

export const api = {
  get: <T>(path: string, signal?: AbortSignal) => request<T>(path, { signal }),
  post: <T>(path: string, body?: unknown) => request<T>(path, { method: "POST", body }),
  put: <T>(path: string, body?: unknown) => request<T>(path, { method: "PUT", body }),
  delete: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};

// ─── Shapes the API returns ──────────────────────────────────────────────────

export type User = {
  id: string;
  email: string;
  role: "ADMIN" | "MEMBER";
  isAdmin: boolean;
  createdAt: string;
  lastLoginAt: string | null;
};

export type Bucket = {
  name: string;
  createdAt: string;
  objectCount: number;
  totalBytes: number;
};

export type StoredObject = {
  key: string;
  name: string;
  size: number;
  etag: string;
  contentType: string;
  lastModified: string;
};

export type Folder = {
  prefix: string;
  name: string;
};

export type ObjectListing = {
  bucket: string;
  prefix: string;
  folders: Folder[];
  objects: StoredObject[];
  isTruncated: boolean;
  nextAfter: string;
};

export type Permission = "read" | "write" | "delete";

export type ScopeRule = {
  bucket: string;
  prefix: string;
  permissions: Permission[];
};

/**
 * What one access key may do.
 *
 * `unrestricted` is kept separate from a rule covering everything, so a key
 * that was never scoped looks different from one deliberately scoped wide.
 * `summary` is rendered by the server, so the console, the audit log and the
 * request log all describe a scope in the same words.
 */
export type AccessScope = {
  unrestricted: boolean;
  rules: ScopeRule[];
  summary: string;
};

export const ALL_PERMISSIONS: Permission[] = ["read", "write", "delete"];

export type Credential = {
  accessKeyId: string;
  description: string;
  createdAt: string;
  lastUsedAt: string | null;
  revoked: boolean;
  revokedAt: string | null;
  scope: AccessScope;
};

export type CreatedCredential = {
  accessKeyId: string;
  secretAccessKey: string;
  description: string;
  endpoint: string;
  region: string;
  scope: AccessScope;
  warning: string;
  snippets: Record<string, string>;
};

export type Invite = {
  id: string;
  email: string;
  role: string;
  createdAt: string;
  expiresAt: string;
};

export type Dashboard = {
  buckets: number;
  objects: number;
  bytesStored: number;
  diskFree: number;
  diskTotal: number;
  durabilityNote: string;
};

export type Traffic = {
  requests24h: number;
  clientErrors: number;
  serverErrors: number;
  errorRate: number;
  bytesIn24h: number;
  bytesOut24h: number;
  daily: { day: string; requests: number; errors: number }[];
};

export type SystemStatus = {
  node: {
    name: string;
    version: string;
    go: string;
    environment: string;
    startedAt: string;
    uptime: number;
  };
  storage: { dataDir: string; diskTotal: number; diskFree: number; readable: boolean; singleCopy: boolean };
  database: {
    reachable: boolean;
    connections: number;
    idleConnections: number;
    maxConnections: number;
    acquiredConns: number;
  };
  endpoints: { s3: string; console: string; region: string; s3Domain: string; virtualHostStyle: boolean };
  config: { resendConfigured: boolean; trustedProxyCount: number };
  counts: { users: number; credentials: number; activeCredentials: number };
  warnings: { area: string; message: string }[];
};

export type AuditEvent = {
  id: number;
  actor: string;
  action: string;
  subjectType: string;
  subject: string;
  detail: Record<string, unknown> | null;
  ip: string | null;
  userAgent: string | null;
  createdAt: string;
};

export type AuditPage = {
  events: AuditEvent[];
  actors: string[];
  actions: { value: string; label: string }[];
  nextBefore: number | null;
};

export type Session = {
  id: string;
  device: string;
  userAgent: string | null;
  ip: string | null;
  createdAt: string;
  lastSeenAt: string;
  expiresAt: string;
  current: boolean;
};

export type CorsRule = {
  allowedOrigins: string[];
  allowedMethods: string[];
  allowedHeaders: string[];
  exposeHeaders?: string[];
  maxAgeSeconds?: number;
};

export type LifecycleRule = {
  id: string;
  prefix: string;
  expireDays: number;
  enabled: boolean;
};

export type BucketSettings = {
  bucket: string;
  publicRead: boolean;
  versioning: boolean;
  corsRules: CorsRule[];
  lifecycleRules: LifecycleRule[];
  updatedAt: string;
  versionedBytes: number;
  versionCount: number;
  publicReadWarning: string;
};

export type ObjectVersion = {
  versionId: string;
  key: string;
  size: number;
  etag: string;
  contentType: string;
  isDeleteMarker: boolean;
  createdAt: string;
  createdBy: string;
  isCurrent: boolean;
};

export type SearchResults = {
  hits: { bucket: string; key: string; size: number; contentType: string; lastModified: string }[];
  truncated: boolean;
  byPrefix: boolean;
};

export type LogEntry = {
  id: number;
  at: string;
  requestId: string;
  surface: "s3" | "console";
  method: string;
  bucket: string;
  key: string;
  path: string;
  status: number;
  errorCode: string;
  reason: string;
  bytesIn: number;
  bytesOut: number;
  durationMs: number;
  accessKeyId: string;
  actor: string;
  clientIp: string;
  userAgent: string;
  sampled: boolean;
};

export type LogPage = { logs: LogEntry[]; nextBefore: number | null };

export type ErrorGroup = {
  errorCode: string;
  reason: string;
  bucket: string;
  accessKeyId: string;
  clientIp: string;
  count: number;
  lastSeen: string;
  /** Named only where the pattern has one sensible reading. */
  likelyCause: string;
};

export type LogSummary = { groups: ErrorGroup[]; windowMinutes: number };

export type ServerEvent = {
  id: number;
  at: string;
  level: "WARN" | "ERROR";
  message: string;
  attributes: Record<string, unknown> | null;
  node: string;
};

export type LogSettings = {
  sampleRate: number;
  slowThresholdMs: number;
  requestRows: number;
  eventRows: number;
  bytes: number;
  note: string;
};

export type Alert = {
  id: number;
  rule: string;
  ruleName: string;
  state: "firing" | "acknowledged" | "resolved";
  severity: "info" | "warning" | "critical";
  summary: string;
  guidance: string;
  detail: Record<string, unknown> | null;
  firedAt: string;
  lastSeenAt: string;
  acknowledgedAt: string | null;
  acknowledgedBy: string | null;
  resolvedAt: string | null;
};

export type AlertPage = { alerts: Alert[]; firing: number; acknowledged: number };

export type AlertRule = {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  severity: "info" | "warning" | "critical";
  settings: Record<string, number> | null;
  updatedAt: string;
};

/**
 * Fired when something changes the alert set, so the sidebar badge updates on
 * the action rather than on its next poll. Acknowledging an alert and watching
 * the badge stay red for a minute reads as a broken button.
 */
export const ALERTS_CHANGED = "pail:alerts-changed";

export function alertsChanged(): void {
  window.dispatchEvent(new Event(ALERTS_CHANGED));
}

export type SetupState = {
  configured: boolean;
  adminEmail: string;
  emailConfigured: boolean;
  hasCredentials: boolean;
  consoleURL: string;
  s3URL: string;
};

// ─── Uploads ─────────────────────────────────────────────────────────────────

/**
 * Uploads one file, reporting progress.
 *
 * XMLHttpRequest rather than fetch: fetch still cannot report upload progress
 * in any browser, and a large upload with no progress bar is indistinguishable
 * from a hung one.
 */
export function uploadObject(
  bucket: string,
  key: string,
  file: File,
  onProgress: (fraction: number) => void,
  signal?: AbortSignal,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open("POST", `/api/buckets/${encodeURIComponent(bucket)}/objects?key=${encodeURIComponent(key)}`);
    request.withCredentials = true;
    if (file.type) {
      request.setRequestHeader("Content-Type", file.type);
    }

    request.upload.addEventListener("progress", (event) => {
      if (event.lengthComputable) {
        onProgress(event.loaded / event.total);
      }
    });

    request.addEventListener("load", () => {
      if (request.status >= 200 && request.status < 300) {
        onProgress(1);
        resolve();
        return;
      }
      let message = `Upload failed (${request.status}).`;
      try {
        const parsed = JSON.parse(request.responseText) as { error?: string };
        if (parsed.error) message = parsed.error;
      } catch {
        // Keep the status-based message.
      }
      reject(new ApiError(request.status, message));
    });

    request.addEventListener("error", () => reject(new ApiError(0, "The upload could not reach the server.")));
    request.addEventListener("abort", () => reject(new ApiError(0, "Upload cancelled.")));

    signal?.addEventListener("abort", () => request.abort());
    request.send(file);
  });
}
