# Paso a paso — tu propia prueba confidencial (comandos reales)

Guía para operar un Confidential Token propio en testnet con tus wallets
de Freighter y leerlo con Umbra. Cada comando es copy-paste; abajo de
cada uno dice qué vas a ver. **Verificado de punta a punta** con la
wallet `SENDER` de abajo (register + deposit aceptados on-chain).

## Contratos (testnet, ya desplegados, no cambian)

- `TOKEN`    = `CBVHVEB4AF2UUDT6M5B4GWQNC3BVC2NDB3YCFW7U2YKNR7JXVVVE7P4J`
- `VERIFIER` = `CDAUQIFN4TNNSNKJZC63BROPLLRBYMRYDMVZB5W7VF2OH5PRHENE7CKW`
- `AUDITOR`  = `CBQEJ2OLNZKM3J2STRBJOJLXXW5XHDZ2ZQ3UV3L23NZTBP55VSNNB3S4`
- SAC (XLM nativo) = `CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC`

## Tus wallets de prueba (testnet, sin valor real)

| Alias CLI | Address | Seed (para importar en Freighter) |
|---|---|---|
| `tw-sender` | `GDCOBTBJMNSODJ3IG4AXU56TTUNHUHWBYQT7MVS7FWA6INWLOEG6LQQQ` | `SB2FS4ZB35KZU2TD53GSG3CXKCV3P4PQMBCG5HJISBNDRDTR62DPANSX` |
| `tw-recipient` | `GBEOK3QVJMRT6MI2JBMDP4HOUJJVM6XRHHO7ONJEVB5GTTBSE4LNDVMF` | `SCLLGPMQQMKAJOFRKVPVI6HUTA35NZMDWENIBMNJJ4HWWOPFHHNXP645` |

Ambas ya están fondeadas con XLM de testnet y ya existen como aliases en
tu `stellar` CLI local (por eso los comandos usan `--source tw-sender`).

## Entorno (pegá esto primero en tu terminal)

```bash
export PATH="$HOME/.nargo/bin:$HOME/.bb:/usr/local/bin:$PATH"
cd ~/Developer/Personal/Code/umbra
export TOKEN=CBVHVEB4AF2UUDT6M5B4GWQNC3BVC2NDB3YCFW7U2YKNR7JXVVVE7P4J
export OZ=~/Developer/Personal/Code/umbra/.ct-demo/circuits
export SENDER_SEED=SB2FS4ZB35KZU2TD53GSG3CXKCV3P4PQMBCG5HJISBNDRDTR62DPANSX
export SENDER_ADDR=GDCOBTBJMNSODJ3IG4AXU56TTUNHUHWBYQT7MVS7FWA6INWLOEG6LQQQ
```

`bin/ct-tool` (ya compilado) hace las dos partes de cripto; `nargo`/`bb`
generan la prueba; `stellar` la envía.

---

## Bloque 0 — importar en Freighter (para el paso de LEER)

Freighter → cambiá a **Testnet** → *Import account* → pegás el seed de
`tw-sender` (y `tw-recipient` si querés ver ambos lados).

## Bloque 1 — REGISTER de SENDER (el único paso con prueba ZK)

```bash
# 1.1 witness: de tu seed + el token, calcula los inputs del circuito
./bin/ct-tool witness "$SENDER_SEED" "$TOKEN" > $OZ/register/Prover.toml

# 1.2 resolver el circuito con esos inputs
( cd $OZ && nargo execute --package circuit_register sender_reg )

# 1.3 generar la prueba (keccak obligatorio, NUNCA --zk)
( cd $OZ && rm -rf target/sender_reg_proof && mkdir -p target/sender_reg_proof && \
  bb prove -s ultra_honk --oracle_hash keccak \
    -b target/circuit_register.json -w target/sender_reg.gz -o target/sender_reg_proof )

# 1.4 empaquetar (payload+proof en XDR) e invocar register, firmando con SENDER
./bin/ct-tool envelope "$SENDER_SEED" "$TOKEN" "$OZ/target/sender_reg_proof/proof" > /tmp/sender_reg.hex
stellar contract invoke --id $TOKEN --source tw-sender --network testnet \
  -- register --account $SENDER_ADDR --auditor_id 0 --data $(cat /tmp/sender_reg.hex)
```
Ves: `Success - Event: [{"symbol":"register"},{"address":"G...SENDER"}]` →
tu prueba fue aceptada on-chain.

## Bloque 2 — DEPOSIT (sin prueba, monto público)

```bash
stellar contract invoke --id $TOKEN --source tw-sender --network testnet \
  -- deposit --from $SENDER_ADDR --to $SENDER_ADDR --amount 30000000
```
Ves: eventos `transfer` (el SAC) + `deposit` (3 XLM = 30000000 stroops).

## Bloque 3 — LEER en Umbra

**Opción navegador (tu Freighter, con tus ojos):**
1. https://umbra-production-d30f.up.railway.app
2. **Connect** → Freighter (`tw-sender`, en Testnet)
3. Token: "Caleb's own CT" (o pegá `$TOKEN`)
4. **Sign & decrypt** → ves `pending` con tu depósito y los tres anillos
   de verificación en verde.

**Opción CLI (cross-check):**
```bash
UMBRA_SECRET_KEY="$SENDER_SEED" ./bin/umbra view --token "$TOKEN"
```

## Bloque 4 (más adelante) — TRANSFER confidencial a RECIPIENT

Requiere la prueba del circuito `transfer` (más compleja: ECDH al
recipient + cifrados de auditor). Se arma igual que register pero con
más inputs; lo montamos cuando llegues aquí.

---

## Notas

- Todo es **testnet** (XLM sin valor). Nunca uses este flujo con una
  wallet de fondos reales: la parte de terminal necesita el seed.
- Los circuitos + VKs viven en `.ct-demo/circuits/` (gitignored). Si se
  borran, se recuperan clonando OZ `feat/confidential-verifier-ultrahonk`
  y copiando `packages/tokens/src/confidential/circuits`.
- El deployer es admin de los registries — bien para demo; producción
  querría multisig + timelock en el verifier (lo recomienda OZ).
