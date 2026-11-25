import { get } from "./http";
import type { Identity } from "./types";

/** The caller's identities (an admin's: everyone's), oldest first. */
export const loadIdentities = (): Promise<Identity[]> =>
  get<{ identities: Identity[] }>("/api/identities").then((body) => body.identities);

/** The most identities a member may have (docs/api.md). */
export const identityMaximum = 100;

/** A name an identity may have: a hostname label. */
export const identityNamePattern = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** How a machine asks for a token of its identity. */
export const tokenCommand = (audience: string): string =>
  `curl -s -H 'Metadata: true' 'http://169.254.169.254/v1/token?audience=${audience}'`;
