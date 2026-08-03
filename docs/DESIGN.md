# Estudio: Indexer de privacidad para Stellar (CT + SPP)

> Fecha: 2026-08-03 · Base: hallazgos verificados contra los repos reales
> (`NethermindEth/stellar-private-payments` y `OpenZeppelin/stellar-contracts`
> rama `feat/confidential-verifier-ultrahonk`, clonados hoy) y la experiencia
> del `trustlesswork-indexer-go` en producción.

## 1. Qué pide el bounty y dónde está el hueco real

El sub-lane pide, como tercer ejemplo: *"a durable event index other builders
can point a wallet at, covering history past the RPC's 7-day window"*.

Hallazgos que definen el posicionamiento:

1. **La wallet de referencia de SPP ya depende de un "bootnode"** para
   sincronizar historial fuera de la ventana de retención del RPC, y
   **Nethermind ya incluye una implementación** (`tools/bootnode`: Rust +
   Postgres + Docker). Es un **cache tonto de páginas de `getEvents`**: solo
   expone `getEvents` y `getLatestLedger` (JSON-RPC compatible), guarda
   páginas crudas, y hace handoff al RPC del usuario con el error `-32002`
   (`fromLedger`) cuando el rango pedido ya cae dentro de retención
   (tip − 5 días). Cache miss = `-32004`.
2. **Su propia documentación enumera los ataques sin mitigarlos**
   (`docs/src/bootnode.md`): historial forjado, omisión/censura selectiva de
   eventos, datos viejos para retrasar el catch-up, `fromLedger` malicioso en
   el handoff, correlación IP/timing. Mitigación oficial: "self-host y/o
   cross-check con varios RPC". O sea: **la integridad quedó como ejercicio
   para el lector**.
3. CT (OpenZeppelin) ni siquiera tiene esa pieza: la wallet necesita los
   ciphertexts de cada transfer para reconstruir su historial de saldo, y no
   hay ningún servicio de historial.

**Posicionamiento del producto:** no "otro bootnode", sino **el índice de
eventos de privacidad durable y *verificable***:

- **Drop-in**: implementa la superficie JSON-RPC del bootnode (incluidos
  `-32002`/`-32004`), así la app de referencia de SPP se le puede apuntar
  **sin cambiar una línea** — demo instantánea y adopción inmediata.
- **Verificable**: responde el "integrity risk" con evidencia, no con
  confianza (ver §6). Nadie más en la categoría va a tener esto.
- **Multi-protocolo**: SPP y CT bajo el mismo techo; el resto de builders del
  hackathon (que estarán construyendo wallets, ejemplos 1 y 2) son los
  usuarios naturales.
- **Forkeable**: `docker compose up` y tienes tu propio índice soberano —
  la mitigación que Nethermind recomienda pero no facilita.

## 2. Qué hay que indexar (esquemas verificados en el código)

### 2.1 SPP — contratos y eventos (crate `contracts/` del repo)

| Contrato | Evento | Topics | Data |
|---|---|---|---|
| `pool` | `NewCommitmentEvent` | `commitment: U256` | `index: u32`, `encrypted_output: Bytes` |
| `pool` | `NewNullifierEvent` | `nullifier: U256` | — |
| `public-key-registry` | `PublicKeyEvent` | `owner: Address` | `encryption_key: Bytes(32)` (X25519), `note_key: Bytes(32)` (BN254) |
| `asp-membership` | `LeafAdded` | — | `leaf: U256`, `index: u64`, `root: U256` |
| `asp-non-membership` | `LeafInserted` | — | `key: U256`, `value: U256`, `root: U256` |
| `asp-non-membership` | `LeafDeleted` | — | `key: U256`, `root: U256` |

Notas de diseño que importan:

- El pool inserta commitments **de a pares** (`insert_two_leaves`) en un
  Merkle tree incremental **Poseidon2/BN254** con historial de raíces
  (`MerkleTreeWithHistory`). El `index` viene en el evento → el orden de
  hojas es reconstruible y verificable.
- `encrypted_output` es lo que la wallet descarga en bloque y descifra por
  trial-decryption con su clave X25519. **El indexer nunca descifra nada.**
- El registry emite un stream global de claves → así se resuelven
  destinatarios de transfers privados.
- Deployments testnet (en `deployments/testnet/deployments.json`): registry
  `CDMGLGZV2S4HW4WKW7ZAYICT73V57QNCVJ5K6A22DVPPJHIQPHFLSGRL`, ASPs, y 2 pools
  (XLM nativo desde ledger 3899359 con blocklist; EURC clásico desde 3899361
  con allowlist+blocklist). Config lista para arrancar.

### 2.2 Confidential Tokens (OZ) — eventos (`packages/tokens/src/confidential/mod.rs`)

`Register{account, auditor_id}`, `Deposit{from, to, amount}` (monto público al
entrar), `Withdraw{from, to, amount, r_e_point…}`, `Transfer{from, to,
r_e_point, v_tilde, …}` (monto SOLO como ciphertext), `Merge{account}`
(rollover del pending balance), `SpenderTransfer`, `SetSpender`,
`RevokeSpender`.

- CT es **confidencialidad, no anonimato**: direcciones visibles, montos
  ocultos (Pedersen sobre Grumpkin, pruebas Noir/UltraHonk). Por eso el
  índice útil es "todos los movimientos de la cuenta X con sus ciphertexts",
  consultable por address — la wallet descifra con su viewing key vía ECDH.
- No hay deployments oficiales estables aún (developer preview) → soporte por
  configuración: cualquier contract ID que el operador declare como CT.

### 2.3 Regla de oro del modelo

**Indexar eventos crudos, sin interpretación destructiva.** Guardar siempre
el XDR original + campos extraídos para consulta. Las vistas derivadas
(hojas, nullifiers, registro) se materializan *a partir de* la tabla cruda y
son regenerables. Esto da: compat bootnode gratis (las páginas `getEvents` se
sirven desde lo crudo), a prueba de cambios de esquema de los contratos, y
re-derivación retroactiva ("redefinir qué nos concierne hacia atrás" — el
criterio que ya habíamos formulado para el indexer de TW).

## 3. Arquitectura

### 3.1 Decisiones ya tomadas (por el usuario)

- Repo **nuevo desde cero**, open source, forkeable y desplegable tal cual.
- **Sin RabbitMQ**. PostgreSQL como única pieza de estado.
- Docker/Compose como forma canónica de correr (indexer + Postgres).
- Lo desplegamos también nosotros como servicio público.

### 3.2 Pipeline (Go, reutilizando los patrones probados del indexer TW)

```
RPC pool (failover) ──► getLedgers stream ──► filtro por contract IDs
                                                    │
                              tx.Result.Successful() ✔ (lección M2)
                                                    │
                        ┌── una TRANSACCIÓN PG por ledger ──┐
                        │  INSERT events (crudo + extraído)  │
                        │  UPSERT vistas derivadas           │
                        │  UPDATE cursor                     │
                        └──────────── COMMIT ────────────────┘
```

Por qué **ingesta por ledger** (`getLedgers`) y no polling de `getEvents`
como hace el bootnode de Nethermind:

- Completitud demostrable por ledger: procesamos el ledger entero, con chequeo
  de éxito de la tx y **continuidad por parent hash** — un fallback que sirva
  otra cadena no puede envenenar el índice (patrón ya escrito en
  `failover.go` del indexer TW).
- `getEvents` upstream es "páginas con forma del proveedor": el bootnode tiene
  que guardar hasta páginas vacías para no romper cadenas de cursores. Nosotros
  generamos las páginas desde datos completos propios.
- Habilita el argumento de historia profunda: `getLedgers` con Infinite Scroll
  llega a génesis en mainnet (medido 2026-07-28: ledger 50.000.000 servido por
  Gateway, ~2 años bajo su `oldestLedger` declarado).

Y el gran simplificador: **cursor y datos en la misma transacción SQL =
exactly-once efectivo**. Toda la clase de problemas más dura del indexer TW
(publisher confirms, backpressure, desfase de confirmaciones — el hallazgo A2)
**desaparece por diseño** al eliminar el broker.

Piezas a portar (reescritas, no copiadas — repo limpio): pool de endpoints con
failover + watchdog de tip congelado; clamp de retención **con el fix ya
diagnosticado** (autoridad = `getLedgers`, no `getHealth` — el bug de
`failover.go:213-224` no se hereda); evidencia de gaps; `replay/backfill` como
subcomando; health/readiness/metrics Prometheus.

### 3.3 Componentes del binario único

1. **Ingester** — el loop de arriba. Único escritor.
2. **Derivador** — dentro de la misma tx: mantiene `pool_leaves`,
   `pool_nullifiers`, `registry_keys`, `asp_state`, `ct_transfers`.
3. **API server** — solo lectura sobre PG (puede escalar horizontal).
4. **Verificador de checkpoints** (fase 2) — reconstruye la raíz Poseidon2
   del árbol de commitments y la compara contra la raíz on-chain
   (`getLedgerEntries`); persiste checkpoints firmados.
5. **Backfill** — rangos históricos vía endpoints archive, en paralelo al vivo.

## 4. Modelo de datos (PG, migraciones embebidas)

```sql
-- verdad cruda, append-only
events(
  id            text PK,        -- determinístico: ledger-tx-op-idx (dedupe/replay)
  network       text,           -- 'testnet' | 'pubnet'
  ledger        bigint, ledger_closed_at timestamptz,
  tx_hash       text, tx_successful bool,
  contract_id   text,
  topics        jsonb,          -- render legible
  topics_xdr    bytea[],        -- crudo
  data          jsonb, data_xdr bytea,
  ingested_at   timestamptz
)                               -- índices: (contract_id, ledger), (ledger)

cursor(network PK, last_ledger, last_ledger_hash, updated_at)
gaps(network, from_ledger, to_ledger, reason, recorded_at)

-- derivadas (regenerables desde events)
pool_leaves(pool_id, leaf_index PK, commitment, encrypted_output, ledger)
pool_nullifiers(pool_id, nullifier PK, ledger)
registry_keys(owner PK, encryption_key, note_key, ledger)   -- última + historial
asp_state(contract_id, kind, key, value, root, ledger)
ct_events(token_id, kind, from_addr, to_addr, amount_public,
          ciphertext jsonb, ledger)                          -- consultable por address
checkpoints(pool_id, ledger, leaf_count, computed_root,
            onchain_root, verified bool, computed_at)        -- fase 2
```

Sin borrados, sin updates destructivos: el guard anti-removals masivos del
indexer TW (A3) **no aplica porque nada se remueve** — el modelo es mejor de
raíz.

## 5. API — las tres superficies

### 5.1 Compat bootnode (JSON-RPC) — la llave de la demo

`POST /` con `getEvents` y `getLatestLedger`, replicando el contrato
documentado en `docs/src/bootnode.md` de SPP: paginación por cursor, error
`-32004` (cache miss / warming up), error `-32002` con
`data.{reason:"retention_threshold", fromLedger}` para handoff. Resultado: la
app oficial de SPP funciona contra nosotros configurando una URL.

### 5.2 REST para builders (lo que el bounty llama "point a wallet at")

```
GET /v1/pools                                   → pools indexados + config
GET /v1/pools/:id/leaves?from_index=&limit=     → hojas EN ORDEN (rebuild del árbol)
GET /v1/pools/:id/outputs?from_index=&limit=    → encrypted_outputs en bloque (trial-decryption)
GET /v1/pools/:id/nullifiers?since_ledger=      → set de gastados (¿mi nota sigue viva?)
GET /v1/pools/:id/root                          → raíz reconstruida + leaf_count + ledger
GET /v1/registry/:address                       → claves de cifrado/nota del destinatario
GET /v1/ct/:token/history/:address              → movimientos CT con ciphertexts
GET /v1/status                                  → cursor, gaps, lag vs tip, redes
+ SSE /v1/stream (tip en vivo: nuevos leaves/nullifiers)     [fase 2]
```

### 5.3 Operación

`/healthz`, `/readyz`, `/metrics` (con `ReadHeaderTimeout`, sin auth solo lo
inofensivo — lecciones M5/M7).

**Privacidad del lado de consulta** (el "privacy risk" del bootnode): la API
se diseña para que el patrón de acceso no delate al usuario — descargas por
**rango completo**, nunca "dame las notas de la clave X"; sin logs de IP por
defecto (`LOG_CLIENT_IP=false`); rate limit por token anónimo; CORS abierto.
La wallet baja todo y descifra localmente — el servidor no puede aprender
qué notas son de quién porque nunca se le pregunta eso.

## 6. Verificabilidad — la respuesta al "integrity risk" (diferenciador)

Cada vector de ataque documentado por Nethermind, con nuestra respuesta:

| Ataque (bootnode.md) | Nuestra respuesta |
|---|---|
| Historial forjado | **Checkpoint verificable**: raíz Poseidon2/BN254 reconstruida desde nuestros eventos == raíz on-chain del pool (leíble por cualquiera vía `getLedgerEntries`). Si forjamos u omitimos un solo commitment, la raíz no cuadra. `gnark-crypto` trae Poseidon2 sobre fr de BN254 en Go; hay que verificar paridad de parámetros con el crate `poseidon2/` de SPP (Horizen Labs) con vectores de prueba cruzados. |
| Omisión / censura selectiva | Los `index` de `NewCommitmentEvent` son consecutivos: un hueco en `leaf_index` es **evidencia matemática de omisión**. Endpoint `/root` + `leaf_count` lo hace chequeable en O(1). |
| Datos viejos para retrasar | `/v1/status` expone lag vs tip firmado por timestamp; el cliente compara con `getLatestLedger` de su propio RPC. |
| `fromLedger` malicioso en handoff | Nuestro handoff se emite solo dentro de la ventana que el RPC del cliente puede auditar; y al ser open source + auto-hosteable, el usuario que no confía corre el suyo. |
| Correlación IP/timing | §5.3: acceso por rangos, sin logs de IP, self-host trivial. |

Frase de pitch: *"No te pedimos confianza: cada página de historial que
servimos es contrastable contra la raíz del contrato."*

## 7. Lecciones de la auditoría TW aplicadas desde el día 1

- **M2**: `tx.Result.Successful()` en el filtro de ingesta (el SDK lo advierte;
  el indexer TW lo omitía).
- **M3**: filtrar `ContractEventTypeContract` y validar `ContractId != nil`.
- **M4**: continuidad de parent hash **permanente**, no de un solo disparo.
- **M1**: redacción de secretos en el log de config (API keys de RPC van en
  el PATH de la URL — p. ej. Validation Cloud).
- **M5**: `ReadHeaderTimeout`, bind configurable.
- **A1 (lección arquitectónica)**: control plane **pull, no push**; sin
  canales de comandos remotos. El único "comando" es re-derivar desde PG.
- **Bajos**: CI con `-race` + `govulncheck` + `gosec` desde el primer commit;
  cero código muerto; `.gitignore` correcto desde el inicio.
- **Catch-up**: no estampar estado del tip con ledgers históricos (M9) — aquí
  ni aplica: todo dato lleva el ledger del evento que lo originó.

## 8. Open source / DX (forkear → desplegar → usar)

- **Licencia** Apache-2.0 (SPP usa una licencia permisiva; alinear).
- **Un binario, una imagen**: Dockerfile multi-stage (distroless), compose con
  `indexer` + `postgres` + volumen; `docker compose up` te da API en :8080.
- **Config 100 % por env** + `deployments.example.json` precargado con los
  contratos testnet de SPP (arranca útil sin editar nada).
- Migraciones embebidas (goose/golang-migrate embed) — sin pasos manuales.
- README con tres audiencias: *wallet builder* (apunta tu app aquí),
  *operador* (fork + deploy soberano), *contribuidor* (agrega un protocolo:
  un decoder + una migración).
- Nuestro despliegue público (Railway, patrón ya dominado) como servicio de
  referencia + el testnet de SPP ya vivo para la demo.

## 9. Plan de ejecución

### Hoy (MVP demostrable)

1. Scaffold: repo, módulos, config, migraciones, CI, Dockerfile, compose.
2. Ingesta: pool RPC con failover simplificado + `getLedgers` → filtro por
   contract IDs → `events` crudo + cursor en una tx.
3. Derivadas mínimas: `pool_leaves`, `pool_nullifiers`, `registry_keys`.
4. REST: `/v1/pools/:id/{leaves,outputs,nullifiers}`, `/v1/registry/:address`,
   `/v1/status`, `/healthz`.
5. Compose end-to-end contra testnet real (pools de SPP ya desplegados).

### Fase 2 (esta semana)

- Compat bootnode JSON-RPC (`getEvents`/`getLatestLedger` + `-32002`/`-32004`)
  y probarla contra la app de SPP.
- Checkpoints verificables (Poseidon2 en Go + comparación on-chain).
- Backfill archive + decoder CT + SSE.

## 10. Riesgos y verificaciones pendientes

1. **Historia profunda en testnet**: el Infinite Scroll de `getLedgers` está
   medido en mainnet; en testnet no, y testnet se resetea periódicamente.
   Los pools de SPP se desplegaron en ledger ~3,9 M (post-reset), así que el
   historial completo *de estos contratos* probablemente sí es alcanzable hoy
   — verificar el día 1 pidiendo el ledger 3899359. Mensaje honesto para la
   demo: "cubrimos desde el deployment del contrato, para siempre".
2. **Paridad Poseidon2**: validar parámetros gnark-crypto vs crate de SPP con
   vectores cruzados antes de prometer el checkpoint verificable.
3. **CT sin deployment oficial estable** (developer preview): soportarlo por
   config y con el decoder listo; la demo fuerte es SPP.
4. **Estado WIP de SPP**: sin auditoría, esquemas pueden cambiar — por eso la
   verdad es el evento crudo y los decoders son reemplazables.
5. **Costo**: mismo patrón ~$5-10/mes ya validado (Railway + RPC gratis con
   failover; Validation Cloud si hace falta cuota).
