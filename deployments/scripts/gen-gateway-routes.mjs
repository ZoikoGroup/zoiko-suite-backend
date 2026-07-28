#!/usr/bin/env node
//
// Regenerate deployments/traefik-dynamic/all-services.yml — the Traefik
// dynamic-file config that exposes every microservice through the single
// gateway port, one path prefix per service.
//
//   node deployments/scripts/gen-gateway-routes.mjs
//
// Run this after adding, removing, or re-porting a service in
// docker-compose.yml. The compose file is the single source of truth: this
// script reads each service's container_name and PORT from it, so the routes
// can never drift from the ports the services actually listen on.
//
// Why the file provider and not Docker labels: 50 services would need 200+
// lines of hand-maintained label YAML, and every port change would need edits
// in two places. Traefik merges every file in /etc/traefik/dynamic, so this
// config sits alongside GTRM's compiled config without interfering with it.

import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const DEPLOYMENTS = join(HERE, "..");
const COMPOSE = join(DEPLOYMENTS, "docker-compose.yml");
const OUT_DIR = join(DEPLOYMENTS, "traefik-dynamic");

// GTRM's regional pools and the quarantine terminator are edge infrastructure
// reached through GTRM's own Host() routers. They all listen on 8080 and must
// not get a path prefix here.
const EXCLUDE = new Set(["eu-pool", "us-pool", "uk-pool", "quarantine-terminator"]);

// GTRM installs a HostRegexp(`^.+$`) catchall at priority 1, which would
// otherwise swallow requests to localhost. Anything above 1 wins; 50 stays
// clear of GTRM's Host() routers at priority 100, so GTRM is unaffected.
const PRIORITY = 50;

/**
 * Pull {composeName, container, port} out of docker-compose.yml.
 *
 * Deliberately a line scanner rather than a YAML parse: it keeps this script
 * dependency-free, and the two keys it needs (container_name, PORT) sit at
 * fixed indentation in this file.
 */
function readServices() {
  const lines = readFileSync(COMPOSE, "utf8").split(/\r?\n/);
  const found = [];
  const containersWithoutPort = [];
  let composeName = null;
  let container = null;
  let port = null;

  // A block is one of our Go services iff it builds from ../services (or from
  // the repo root, as search-indexer-svc does). Infra runs from `image:` and
  // legitimately has no PORT, so build context — not the absence of a PORT — is
  // what distinguishes "should be routed" from "not an application service".
  let isGoService = false;

  const flushPending = () => {
    // A Go service we saw but never found a PORT for. Recorded rather than
    // ignored — a silently dropped service means a route that quietly does not
    // exist, which is far worse than a loud failure here.
    if (isGoService && container && !port) containersWithoutPort.push(container);
  };

  for (const line of lines) {
    const service = /^ {2}([A-Za-z0-9_.-]+):\s*$/.exec(line);
    if (service) {
      flushPending();
      composeName = service[1];
      container = null;
      port = null;
      isGoService = false;
      continue;
    }
    if (/^ {6}context:\s*(\.\.\/services\/|\.\.\s*$)/.test(line)) isGoService = true;
    const cn = /^ {4}container_name:\s*(\S+)\s*$/.exec(line);
    if (cn) container = cn[1];

    // Trailing comments are allowed — audit-svc's PORT line carries one
    // explaining its remap, and requiring end-of-line silently dropped it.
    const p = /^ {6}PORT:\s*"?(\d+)"?\s*(?:#.*)?$/.exec(line);
    if (p) port = Number(p[1]);

    if (composeName && container && port) {
      found.push({ composeName, container, port });
      container = null;
      port = null;
    }
  }
  flushPending();

  const unrouted = containersWithoutPort.filter((c) => !EXCLUDE.has(c));
  if (unrouted.length > 0) {
    console.error(
      `Refusing to generate: ${unrouted.length} container(s) have no PORT and would ` +
        `be missing from the gateway:\n  ${unrouted.join("\n  ")}\n` +
        `Give them a PORT in docker-compose.yml, or add them to EXCLUDE.`,
    );
    process.exit(1);
  }

  return found;
}

const services = readServices()
  .filter((s) => !EXCLUDE.has(s.composeName))
  .sort((a, b) => a.container.localeCompare(b.container));

if (services.length === 0) {
  console.error("No services found — has docker-compose.yml's indentation changed?");
  process.exit(1);
}

const header = `# GENERATED — do not hand-edit.
# Regenerate: node deployments/scripts/gen-gateway-routes.mjs
#
# One port fronts every service, one path prefix each:
#
#     http://localhost:\${GATEWAY_PORT}/<service-name>/<the service's own path>
#
# e.g.  /purchase-order-svc/v1/purchase-orders  -> purchase-order-svc:8129/v1/purchase-orders
#       /authorization-svc/v1/authorize         -> authorization-svc:8089/v1/authorize
#       /policy-svc/healthz                     -> policy-svc:8085/healthz
#
# LOCAL DEVELOPMENT ONLY. These routes carry NO gateway-auth middleware, so
# every service is reachable unauthenticated through this port. That is
# deliberate — the purpose is manual status checking — and must never be
# deployed. Production routing is GTRM's compiled config: Host()-based and
# ForwardAuth-enforced.
#
# priority ${PRIORITY} beats GTRM's HostRegexp catchall (priority 1) and stays below
# its Host() routers (priority 100), so GTRM behaviour is unchanged.

`;

const out = [header, "http:", "    routers:"];

for (const s of services) {
  out.push(`        svc-${s.container}:`);
  out.push(`            rule: PathPrefix(\`/${s.container}\`)`);
  out.push(`            entryPoints:`);
  out.push(`                - web`);
  out.push(`            service: svc-${s.container}`);
  out.push(`            middlewares:`);
  out.push(`                - strip-${s.container}`);
  out.push(`            priority: ${PRIORITY}`);
}

out.push("    middlewares:");
for (const s of services) {
  // Strip the prefix so the service receives the path it actually registered.
  out.push(`        strip-${s.container}:`);
  out.push(`            stripPrefix:`);
  out.push(`                prefixes:`);
  out.push(`                    - /${s.container}`);
}

out.push("    services:");
for (const s of services) {
  out.push(`        svc-${s.container}:`);
  out.push(`            loadBalancer:`);
  out.push(`                servers:`);
  out.push(`                    - url: http://${s.container}:${s.port}`);
}

mkdirSync(OUT_DIR, { recursive: true });
writeFileSync(join(OUT_DIR, "all-services.yml"), out.join("\n") + "\n", "utf8");

const routes = [
  "# Gateway route index — generated by deployments/scripts/gen-gateway-routes.mjs",
  "#",
  "# Every service is reachable on the single gateway port at its path prefix.",
  "# The direct port still works and is useful when the gateway itself is suspect.",
  "",
  "| Path prefix | Service | Direct port |",
  "| --- | --- | --- |",
  ...services.map((s) => `| \`/${s.container}\` | ${s.container} | ${s.port} |`),
  "",
].join("\n");
writeFileSync(join(OUT_DIR, "ROUTES.md"), routes, "utf8");

console.log(`${services.length} services routed`);
console.log(`  ${join(OUT_DIR, "all-services.yml")}`);
console.log(`  ${join(OUT_DIR, "ROUTES.md")}`);
