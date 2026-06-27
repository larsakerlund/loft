// The daemon calls the console makes: who is signed in, and shipping a set of files to a subdomain.
// Every call is same-origin to /api, which nginx forwards to loftd (the browser never holds creds).

import type { DeployFile } from "./files";

export interface Identity {
  id: string;
  name: string;
  email: string;
}

/** The signed-in user, or null if /api/me is unreachable (e.g. local auth-off before boot). */
export async function fetchIdentity(): Promise<Identity | null> {
  try {
    const res = await fetch("/api/me");
    if (!res.ok) return null;
    return (await res.json()) as Identity;
  } catch {
    return null;
  }
}

/** The host user sites live under, e.g. ".loft.example.com" (a leading-dot suffix). */
export function siteSuffix(): string {
  return `.${window.location.host}`;
}

/** The full URL a deployed site will serve at. */
export function siteUrl(site: string): string {
  return `${window.location.protocol}//${site}${siteSuffix()}`;
}

export interface DeployResult {
  url: string;
}

/** Thrown when the target subdomain already has a site and the caller didn't confirm overwrite. */
export class SiteExistsError extends Error {
  constructor(public readonly site: string) {
    super(`site "${site}" already exists`);
    this.name = "SiteExistsError";
  }
}

/**
 * Upload the files to a subdomain. loftd writes them to the site's storage and the URL goes live.
 * Without overwrite, deploying onto an existing site throws SiteExistsError so the console can ask.
 */
export async function deploy(site: string, files: DeployFile[], overwrite = false): Promise<DeployResult> {
  const form = new FormData();
  form.set("site", site);
  form.set("overwrite", overwrite ? "true" : "false");
  for (const { path, file } of files) {
    // The part filename carries the site-relative path so loftd lays the tree out as-is.
    form.append("files", file, path);
  }
  const res = await fetch("/api/deploy", { method: "POST", body: form });
  if (res.status === 409) throw new SiteExistsError(site);
  if (!res.ok) {
    const detail = (await res.text()).trim();
    throw new Error(detail || `deploy failed (${String(res.status)})`);
  }
  return { url: siteUrl(site) };
}
