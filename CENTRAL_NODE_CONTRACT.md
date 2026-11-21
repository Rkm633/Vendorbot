# Central Brain Node Integration Contract & Complete Bot Architecture (`v2.3`)

This document is the complete technical specification and architectural guide for the **Stateless Vendor WhatsApp Auto-Responder Bot Node** (Go + WhatsMeow) and how it communicates bi-directionally with your **Multi-Vendor Mall + Individual Storefront Central Brain Node** using an **event-driven, zero-polling, zero-chat-sync** architecture.

Every vendor deploys this repository on **Render, Railway, Koyeb, Fly.io, or Cloud Run** with **zero database** and **only 2 required environment variables**:

```env
CENTRAL_NODE_URL=https://api.your-mall-central-node.com
VENDOR_API_KEY=vnd_live_xxxxxxxxxxxxxxxxxxxxxxxx
```

Optional environment variables:
- `PORT` (defaults to `3000`)
- `BOT_PUBLIC_URL` (optional — auto-detected automatically from Render/Railway/Koyeb/Fly env vars or incoming `Host` / `X-Forwarded-Host` headers)

---

## 1. High-Level Architecture & Core Guarantees (`v2.3`)

1. **Zero Database (100% RAM-First Stateless Node)**:
   - The bot stores **no permanent database** locally. All vendor details, AI config, product catalog (variants, stock, HTTPS image URLs), customers, active carts, orders (capped at latest 500), and leads live in thread-safe RAM for `0ms` lookup speed.
   - On server startup or container restart, the bot sends **one single** bootstrap request to Central Brain Node to hydrate RAM.
2. **Zero Customer Chat Sync (Strictly Bot RAM Only — Max 10 Messages)**:
   - Incoming and outgoing customer WhatsApp messages are **never** sent to Central Brain Node.
   - On startup bootstrap, the bot **never** requests or loads historical customer chats (`recent_chats`) from Central Brain Node.
   - The bot keeps **strictly the last 10 messages per customer** in local RAM so the AI responder remembers recent conversation context; anything older than 10 messages is immediately pruned from RAM.
3. **Zero Periodic Polling & Zero Recurring Heartbeat**:
   - There is **no background polling timer**, **no periodic bootstrap loop**, and **no recurring `/api/vendor-bot/heartbeat` request**.
   - Central Brain Node is called **once at server startup** (and only again if the vendor manually clicks the **Sync** button on the bot dashboard).
   - After startup, all live changes (business details, open/holiday mode, AI settings/keys, products/stock/images, website carts, website customers, payment verification, order confirmation, delivery status) are pushed by Central Brain Node to the bot via inbound `/api/sync/*` webhooks.
4. **WhatsApp Session Synced ONLY on 3 Lifecycle Events (Never Per Message)**:
   - There is **no per-message or debounced session upload timer** and **no upload on routine reconnects or history sync**.
   - The bot persists or deletes the WhatsMeow session on Central Brain Node **strictly on 3 occasions**:
     1. **First-time login completion** (QR code scan or 8-digit phone pairing) → `POST /api/vendor-bot/whatsapp/session`.
     2. **Vendor logout / unlink or phone logout** → `DELETE /api/vendor-bot/whatsapp/session`.
     3. **Graceful server shutdown / container restart** (after cleanly disconnecting the WhatsApp WebSocket first) → `POST /api/vendor-bot/whatsapp/session`.
   - Safety guard: the bot **never** overwrites a valid Central session with an empty or logged-out blob.
5. **Strict Essential-Business-Events-Only Outbound Traffic**:
   - Ordinary WhatsApp conversations make **zero HTTP calls** to Central Brain Node.
   - Outbound HTTP requests are sent **only** on:
     1. Startup bootstrap (once on boot + manual dashboard **Sync**).
     2. WhatsApp session lifecycle (the 3 triggers above).
     3. **Customer details update (change-detected only)**: `POST /api/vendor-bot/events/customer` ONLY when a brand-new customer arrives OR when `full_name`, `wa_number`, `contact_phone`, `delivery_address`, `latitude`, or `longitude` actually changed vs RAM.
     4. **Cart update**: `POST /api/vendor-bot/events/cart` ONLY when items are added, updated, removed, or cleared.
     5. **Order submit & cancel**: `POST /api/vendor-bot/events/order` on checkout confirmation (`order.requested`) or cancellation (`order.upserted`).
     6. **On-demand website/mall auto-login link**: `POST /api/vendor-bot/events/magic-link` ONLY when a customer explicitly asks for the store/mall link.
6. **Zero UTR & Zero Payment Screenshot Verification**:
   - The bot **never** parses UTR numbers, **never** OCRs/scans customer payment screenshots, and **never** marks any order as `PAID` on its own.
   - If a customer uploads a screenshot or claims payment is done, the bot politely informs them that the Central Payment System verifies payments automatically and sends final confirmation + PDF receipt once verified.
7. **100% Central-Brain-Controlled Checkout & Online Payments**:
   - When a WhatsApp customer confirms checkout (`COD` or `ONLINE`), the bot sends **only** cart items (with variants & qty), customer name, phone, delivery address, GPS coordinates (if shared), and payment mode to `POST /api/vendor-bot/events/order`.
   - Central Brain creates the authoritative order, generates any payment link or payment QR image URL, and returns them synchronously.
   - Bot delivers Central Brain's order reference, payment link, and QR image to the customer on WhatsApp.
   - Once Central Brain verifies online payment (or confirms COD), Central Brain calls the bot's `POST /api/sync/order` webhook (`event: "order.paid"` or `"order.confirmed"`), which triggers the bot to clear the customer's cart and deliver the final WhatsApp confirmation + 80mm thermal PDF receipt.
8. **WhatsApp Login Strictly on the Vendor Bot Dashboard**:
   - There are **no** remote WhatsApp login/pairing APIs exposed to the Mall Business Portal.
   - On startup, the bot checks `whatsapp_session` returned by Central Brain's bootstrap API:
     - **If `has_session: true` with a valid `session_blob_base64`** → restores the ephemeral SQLite session file at `/tmp/vendor_wa_session.db` and auto-reconnects silently without asking for login.
     - **If `has_session: false`** → enters `Needs_Login` mode and shows QR code scan + 8-digit phone pairing **exclusively on this Vendor Bot's own dashboard**.
9. **Memory Protection for 512MB Free-Tier Containers**:
   - Go GC tuned (`GOGC=20`, memory limit `400 MiB`), per-customer chat history capped at `10` messages in RAM (idle AI windows evicted after `15m`), product image + `96px` JPEG thumbnail RAM cache capped at `40` entries (`<= 2.5 MB` each, `30m` TTL, LRU eviction), orders capped at `500`, outbound async queue capped at `512` jobs with a `256`-item bounded retry buffer.

---

## 2. How the Bot Works End-to-End (Complete Lifecycle)

### Phase A: Startup & One-Time Hydration Flow
1. Container boots → reads `CENTRAL_NODE_URL` & `VENDOR_API_KEY` → auto-detects `bot_public_url` from platform env vars (`RENDER_EXTERNAL_URL`, `RAILWAY_STATIC_URL`, `RAILWAY_PUBLIC_DOMAIN`, `KOYEB_PUBLIC_DOMAIN`, `FLY_APP_NAME`, `KOYEB_APP_NAME`, etc.).
2. Calls `POST {CENTRAL_NODE_URL}/api/vendor-bot/bootstrap` **once** (up to 3 startup retries with `GET` fallback).
3. Hydrates RAM atomically (`vendor`, `ai`, `payment`, `products`, `customers`, `active_carts`, `orders`, `whatsapp_session`). **No `recent_chats` are fetched.**
4. Initializes WhatsApp engine:
   - Restores `session_blob_base64` into `/tmp/vendor_wa_session.db` if `has_session: true` and auto-connects WebSocket silently.
   - Otherwise sets status to `Needs_Login` for dashboard QR / 8-digit phone pairing.
5. Starts **only** the local WhatsApp WebSocket keep-alive monitor (every `45s`, local-only — makes zero HTTP calls to Central Node).
6. All subsequent live updates arrive via Central Brain pushing webhooks to `{bot_public_url}/api/sync/*`.

### Phase B: Incoming WhatsApp Message → AI Processing → Customer Reply Flow
When a WhatsApp message arrives on the vendor's paired number:

1. **Anti-Ban Event Filtering**:
   - Strictly ignores messages sent by self (`IsFromMe`), group chats (`@g.us`), WhatsApp Channels (`newsletter`), and Status broadcasts (`status@broadcast`).
   - Resolves WhatsApp `@lid` identifiers to real phone numbers via the cached `whatsmeow_lid_map` and normalizes 10-digit Indian numbers to `91XXXXXXXXXX`.
2. **Change-Detected Customer Check & Read Receipt**:
   - Checks customer against RAM using `UpsertCustomerWithChangeCheck`.
   - Pushes `POST /api/vendor-bot/events/customer` **ONLY** if the customer is brand-new OR `full_name`, `wa_number`, `contact_phone`, `delivery_address`, `latitude`, or `longitude` changed. Existing customers chatting normally trigger **zero** Central Node requests.
   - Sends WhatsApp read receipt (blue ticks) asynchronously.
3. **Message Type Routing**:
   - **GPS Location Pin (`LocationMessage` / `LiveLocationMessage`)**:
     - Reverse-geocodes coordinates via OpenStreetMap Nominatim.
     - Saves `latitude`, `longitude`, and address in RAM and pushes `POST /api/vendor-bot/events/customer` (only if changed).
     - Computes Haversine distance from store coordinates (`vendor.latitude`, `vendor.longitude`) against `max_delivery_radius_km`, `free_delivery_radius_km`, and `delivery_charge_amount`.
     - Immediately replies to the customer with verified address, distance in km, and exact delivery fee (or out-of-range notice).
   - **Voice Note / Audio (`AudioMessage`)**:
     - Downloads `.ogg` audio via WhatsMeow.
     - If a Groq API key (`gsk_...`) exists in the vendor's key pool, transcribes the audio using Groq Whisper (`whisper-large-v3-turbo`) and feeds `[Customer Voice Note]: <transcript>` into the AI pipeline.
     - Otherwise feeds a polite fallback prompt asking the customer to type if anything is unclear.
   - **Image Message (`ImageMessage`)**:
     - **Does NOT run OCR or UTR extraction.** Passes `[Customer shared an image ...]` (plus caption if present) to the AI so it assists with product inquiries and reminds the customer that online payments are automatically verified by the Central system.
   - **Text / Caption / WhatsApp Catalog Inquiry**:
     - Extracted as text and passed to the debouncer.
4. **Governance & Store Operating Gates** (checked before calling AI):
   - **Central Admin / Subscription / Vendor Auto-Reply Gate**:
     - Checks `admin_ai_auto_reply_allowed`, `subscription_active` + `subscription_valid_until`, and `ai.auto_reply`.
     - If paused/expired → stores incoming message in local RAM only (max 10), updates live dashboard SSE stream, and **skips auto-reply** (zero Central Node calls).
   - **Store Closed / Holiday Mode Gate**:
     - Checks `holiday_mode` and `is_open`.
     - If closed → replies with `holiday_message` (or closed notice), `operating_hours`, and `next_open_time`. Rate-limited to **once every 20 minutes per customer**.
5. **Anti-Ban 2.5-Second Debouncer & Human Override**:
   - Combines rapid consecutive messages from the same customer over `2.5s` into a single turn.
   - Enforces a per-customer concurrency lock (no parallel overlapping AI turns for the same phone).
   - Checks `MessageReadByHuman`: if the vendor already opened/read the message on their phone during the debounce window, the bot steps aside so the human owner can reply.
   - Emits natural WhatsApp `"typing..."` presence indicators while the AI thinks.
6. **Octopus Agentic AI Brain (RAM-Powered + Multi-Key Failover)**:
   - Builds the system prompt directly from RAM:
     - Store identity (`business_name`, `business_type`, `language`, `store_policies`, custom `system_prompt`).
     - Delivery rules (`require_customer_location`, radii, fee, `estimated_delivery_time`, `min_order_amount`, `operating_hours`).
     - Payment policy (`allow_cod` true/false + strict zero-UTR / Central-controlled checkout rules).
     - **Live In-Stock Catalog** (automatically excludes out-of-stock products and out-of-stock variants) + keyword-matched RAG subset + current `shopping_cart`, `customer_profile`, `current_state`, and **last 10 RAM-only chat turns**.
   - Supported LLM Providers: `groq`, `gemini`, `openai` (alias `chatgpt`), `deepseek`, `openrouter` (also auto-detected by API key prefix: `gsk_` → Groq, `sk-` → OpenAI, `AIzaSy` → Gemini).
   - **Multi-Key Rotation & Automatic Failover**: Combines `ai.api_key`, `ai.api_keys[]`, and `ai.backup_api_keys[]`, rotates round-robin across turns, and immediately switches to the next key on HTTP `429` rate limit, `401`, or timeout.
   - Forces strict raw JSON output:
     ```json
     {
       "ai_reply_text": "Formatted WhatsApp message in vendor language",
       "actions": [
         { "hand": "<action_name>", "payload": { ... } }
       ]
     }
     ```
7. **Action Execution ("Octopus Hands")**:
   - `send_catalog_images`: Filters in-stock RAM products by product name/category (`"all"` or keyword), downloads HTTPS image URLs from Central's catalog (with `30m` / `40`-entry LRU RAM cache + `96px` JPEG thumbnail generation), and sends up to `6` images (`350ms` spacing) with formatted caption (**Product Name, Price/Unit, Category, Available Variants, Description**). Makes zero calls to Central Node.
   - `update_cart`: Validates product & variant stock quantity in RAM; rejects out-of-stock or excess quantity items (appending a clear `⚠️ Stock Update` to the reply), updates RAM cart, and pushes `POST /api/vendor-bot/events/cart` to Central Brain.
   - `extract_profile`: Updates customer `full_name`, `delivery_address`, `contact_phone` in RAM and pushes `POST /api/vendor-bot/events/customer` **only if any profile field actually changed**.
   - `submit_order` (with `payment_mode: "COD" | "ONLINE"`):
     - Validates non-empty cart, `min_order_amount`, delivery address present, `allow_cod` (if COD), and delivery radius (if GPS coords exist).
     - Makes a **synchronous** `POST /api/vendor-bot/events/order` (`event: "order.requested"`) call to Central Brain.
     - Caches Central's returned `order_id`, `order_status`, `payment_link`, and `payment_qr_url` in RAM.
     - If `ONLINE` and `payment_qr_url` is returned → downloads & sends the payment QR image on WhatsApp with caption.
     - Replaces `ai_reply_text` with Central's order reference, payment link, and instructions.
   - `generate_store_link`: Generates a cryptographically random 24-byte hex `auth_token` (24h expiry), pushes `POST /api/vendor-bot/events/magic-link` to Central Brain, and sends the customer **both** their auto-login Vendor Storefront URL and Multi-Vendor Mall URL.
   - `clear_cart`: Clears customer's RAM cart and pushes empty cart sync to Central Brain **only if the cart previously had items**.
   - `cancel_order`: Marks active order `CANCELLED` in RAM, pushes `POST /api/vendor-bot/events/order` (`event: "order.upserted"`), and clears cart.
   - `escalate_to_human`: Saves lead in local RAM and sends a WhatsApp alert directly to `vendor.notification_phone` while continuing to assist the customer (zero Central HTTP call).
8. **Reply Delivery & Local-Only Chat Memory (Zero Central Sync)**:
   - Sends `ai_reply_text` as a WhatsApp quoted reply to the customer's message.
   - Streams message to the Vendor Bot Dashboard Live Chat via SSE (`/api/chat/stream`).
   - Saves customer & assistant turns strictly in local RAM (max `10` messages per customer; older messages automatically dropped). **Never pushes chats or session blobs to Central Brain Node.**

### Phase C: Graceful Redeploy / Shutdown Flow
On `SIGTERM` / `SIGINT` (Render / Railway / Koyeb rolling redeploy):
1. Stops accepting new HTTP requests (`4s` timeout).
2. Flushes all pending outbound business events + retry buffer to Central Brain (`6s` timeout).
3. Cleanly disconnects the WhatsMeow WebSocket **first** (prevents dual-container session conflict / forced logout).
4. Uploads the final validated gzip+base64 session snapshot **only if legitimately paired** (never overwrites a valid Central session with an empty or logged-out blob).

---

## 3. Authentication (Both Directions)

Every HTTP call between the Vendor Bot Node and Central Brain Node authenticates with `VENDOR_API_KEY`:

```http
Authorization: Bearer <VENDOR_API_KEY>
X-Vendor-API-Key: <VENDOR_API_KEY>
Content-Type: application/json
```

Inbound webhooks sent from Central Brain to the Vendor Bot accept the key via `X-Vendor-API-Key`, `Authorization: Bearer <key>`, or `?api_key=<key>` (verified using constant-time comparison).

---

## 4. Outbound API: Strict Business-Events-Only (Bot → Central Brain Node)

The bot **only** calls the following 6 endpoints on Central Brain Node. No polling, no heartbeat, no chat sync.

| # | Endpoint | Trigger Condition |
|---|---|---|
| 1 | `POST /api/vendor-bot/bootstrap` | **Once** on container startup / restart + manual dashboard **Sync** button only |
| 2 | `POST /api/vendor-bot/whatsapp/session` | **Strictly 2 save triggers**: (a) first-time QR/pairing login completion, (b) graceful shutdown after disconnecting WS |
| 3 | `DELETE /api/vendor-bot/whatsapp/session` | **Strictly 1 delete trigger**: vendor explicit logout/unlink or phone logout |
| 4 | `POST /api/vendor-bot/events/customer` | **Change-detected only**: brand-new customer OR `full_name` / `wa_number` / `contact_phone` / `delivery_address` / `latitude` / `longitude` changed |
| 5 | `POST /api/vendor-bot/events/cart` | **Cart mutations only**: add, update, remove, or clear (when non-empty) |
| 6 | `POST /api/vendor-bot/events/order` | **Order lifecycle only**: checkout confirmation (`order.requested`, synchronous) or cancellation (`order.upserted`) |
| 7 | `POST /api/vendor-bot/events/magic-link` | **On-demand only**: customer explicitly asks for website/mall auto-login link |

---

### 4.1 Startup Bootstrap (One-Time on Boot + Manual Sync Button)
- **Endpoint**: `POST {CENTRAL_NODE_URL}/api/vendor-bot/bootstrap` *(fallback: `GET` if `POST` fails)*
- **When called**: Once when the server starts or restarts, and manually if the vendor clicks **Sync** on the bot dashboard. **Never called periodically in the background.**
- Central Brain should store `bot_public_url` from this request so it knows where to push `/api/sync/*` webhooks.

**Request Payload (Bot → Central):**
```json
{
  "bot_public_url": "https://vendor-shop-bot.onrender.com",
  "node_version": "2.3-zero-polling-zero-chat-sync",
  "timestamp": "2026-06-01T10:00:00Z"
}
```

**Expected Response (`200 OK`, either top-level JSON or wrapped inside `{ "status": "success", "data": { ... } }`):**
> **Important:** Do **NOT** include `recent_chats` in the bootstrap payload — the bot keeps chats strictly in local RAM (last 10 per customer) and ignores historical chats on boot.

```json
{
  "status": "success",
  "data": {
    "vendor": {
      "vendor_id": "vnd_1042",
      "username": "sharma_sweets",
      "business_name": "Sharma Sweets & Mart",
      "business_type": "Sweets & Snacks",
      "language": "Hinglish",
      "store_policies": "Fresh daily batch. Easy replacement within 24 hours.",
      "store_slug": "sharma-sweets",
      "store_url": "https://sharma-sweets.yourmall.com",
      "mall_url": "https://yourmall.com",
      "notification_phone": "919876500001",
      "latitude": 26.8467,
      "longitude": 80.9462,
      "max_delivery_radius_km": 12,
      "free_delivery_radius_km": 3,
      "delivery_charge_amount": 35,
      "require_customer_location": false,
      "allow_cod": true,
      "is_open": true,
      "holiday_mode": false,
      "holiday_message": "",
      "operating_hours": "09:00 AM - 10:00 PM",
      "next_open_time": "Tomorrow 09:00 AM",
      "min_order_amount": 199,
      "estimated_delivery_time": "30-45 mins",
      "subscription_active": true,
      "subscription_valid_until": "2027-01-01T00:00:00Z",
      "admin_ai_auto_reply_allowed": true,
      "admin_pause_reason": ""
    },
    "ai": {
      "provider": "groq",
      "api_key": "gsk_primary_key",
      "api_keys": ["gsk_primary_key"],
      "backup_api_keys": ["gsk_backup_key_1", "gsk_backup_key_2"],
      "model": "llama-3.3-70b-versatile",
      "temperature": 0.7,
      "system_prompt": "Always greet warmly and mention today's fresh sweets.",
      "auto_reply": true,
      "import_catalog": true
    },
    "products": [
      {
        "id": "p_101",
        "name": "Kaju Katli Royal Box",
        "category": "Signature Sweets",
        "unit_type": "box",
        "price": 520,
        "sale_price": 480,
        "mrp": 560,
        "description": "Pure cashew diamond fudge made with silver leaf.",
        "stock_qty": 24,
        "in_stock": true,
        "image_url": "https://cdn.yourmall.com/kaju-katli-1.jpg",
        "images": [
          "https://cdn.yourmall.com/kaju-katli-1.jpg",
          "https://cdn.yourmall.com/kaju-katli-2.jpg"
        ],
        "thumb_url": "https://cdn.yourmall.com/kaju-katli-thumb.jpg",
        "variants": [
          { "name": "250g", "price": 260, "sale_price": 245, "stock_qty": 14, "in_stock": true, "sku": "KK-250" },
          { "name": "500g", "price": 520, "sale_price": 480, "stock_qty": 10, "in_stock": true, "sku": "KK-500" },
          { "name": "1kg", "price": 980, "sale_price": 920, "stock_qty": 0, "in_stock": false, "sku": "KK-1000" }
        ]
      }
    ],
    "customers": [
      {
        "wa_number": "919876543210",
        "jid": "919876543210@s.whatsapp.net",
        "full_name": "Aarav Verma",
        "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
        "contact_phone": "919876543210",
        "latitude": 26.8512,
        "longitude": 80.9981,
        "source": "website"
      }
    ],
    "active_carts": {
      "919876543210": [
        {
          "product_id": "p_101",
          "product_name": "Kaju Katli Royal Box",
          "unit_price": 480,
          "unit_type": "box",
          "qty": 2,
          "selected_variant": "500g",
          "line_total": 960,
          "source": "website"
        }
      ]
    },
    "orders": [
      {
        "order_id": "ORD900410",
        "customer_phone": "919876543210",
        "customer_name": "Aarav Verma",
        "delivery_phone": "919876543210",
        "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
        "items": [],
        "subtotal": 480,
        "delivery_fee": 0,
        "total_amount": 480,
        "payment_mode": "ONLINE",
        "payment_status": "PAID",
        "order_status": "DELIVERED",
        "source": "website"
      }
    ],
    "whatsapp_session": {
      "has_session": true,
      "device_jid": "919876500001:4@s.whatsapp.net",
      "phone": "919876500001",
      "push_name": "Sharma Sweets",
      "status": "Connected",
      "session_blob_base64": "<gzip+base64-encoded-sqlite-session-file>"
    }
  }
}
```
> Note: `vendor` may also be returned under the key `business`. `variants` accepts either structured objects (`[{"name":"500g","price":480,"stock_qty":10,"in_stock":true}]`) or plain strings (`["250g","500g"]`). If the vendor has never linked WhatsApp before, return `"whatsapp_session": { "has_session": false, "status": "Needs_Login" }`.

---

### 4.2 WhatsMeow Session Save & Delete (3 Triggers Only)
- **Save Session**: `POST {CENTRAL_NODE_URL}/api/vendor-bot/whatsapp/session`
  - **Strictly called ONLY on 2 save occasions**:
    1. **First-time login completion** (immediately after QR code scan or 8-digit phone pairing succeeds — deduplicated so it uploads once).
    2. **Graceful server shutdown / restart** (synchronously after cleanly disconnecting the WhatsMeow WebSocket).
  - **Never** called per chat message, never on a debounce timer, and never on routine reconnect/history sync.
  - **Safety Guard**: Verifies the local SQLite file is `>= 4096` bytes with valid `SQLite format 3` header and non-empty `device_jid` before uploading. Never overwrites a valid Central session with an empty or logged-out blob.

**Request Payload (Bot → Central):**
```json
{
  "device_jid": "919876500001:4@s.whatsapp.net",
  "phone": "919876500001",
  "push_name": "Sharma Sweets",
  "status": "Connected",
  "has_session": true,
  "session_blob_base64": "<gzip-compressed-base64-sqlite-bytes>",
  "bot_public_url": "https://vendor-shop-bot.onrender.com",
  "updated_at": "2026-06-01T10:05:00Z"
}
```
> Central Brain should store `session_blob_base64` as a `TEXT` / `BYTEA` / blob column per vendor and return it unchanged inside `bootstrap.data.whatsapp_session`.

- **Delete Session (3rd Trigger — Explicit Logout / Unlink / Phone Logout)**:
  - `DELETE {CENTRAL_NODE_URL}/api/vendor-bot/whatsapp/session`
  - *(Fallback if `DELETE` returns non-2xx)*: `POST {CENTRAL_NODE_URL}/api/vendor-bot/whatsapp/session` with:
    ```json
    { "has_session": false, "status": "Disconnected", "session_blob_base64": "" }
    ```

---

### 4.3 Order Submission (Synchronous Checkout) & Order Cancellation
- **Endpoint**: `POST {CENTRAL_NODE_URL}/api/vendor-bot/events/order`

#### Case A: Synchronous WhatsApp Checkout Request (`event: "order.requested"`)
Triggered when a WhatsApp customer confirms COD or Online checkout. The bot waits synchronously for Central Brain's response so it can immediately send the authoritative `order_id`, `payment_link`, `payment_qr_url`, and `payment_instructions` back to the customer. (If Central Brain is temporarily unreachable, the bot queues the request in its retry buffer and sends the customer a polite pending-confirmation message.)

**Request Payload (Bot → Central):**
```json
{
  "event": "order.requested",
  "source": "whatsapp",
  "customer_phone": "919876543210",
  "jid": "919876543210@s.whatsapp.net",
  "customer_name": "Aarav Verma",
  "delivery_phone": "919876543210",
  "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
  "latitude": 26.8512,
  "longitude": 80.9981,
  "payment_mode": "ONLINE",
  "items": [
    {
      "product_id": "p_101",
      "product_name": "Kaju Katli Royal Box",
      "unit_price": 480,
      "unit_type": "box",
      "qty": 2,
      "selected_variant": "500g",
      "line_total": 960,
      "source": "whatsapp"
    }
  ],
  "subtotal": 960,
  "delivery_fee": 0,
  "total_amount": 960,
  "timestamp": "2026-06-01T10:15:00Z"
}
```

**Expected Central Brain Response (`200 OK`, top-level or inside `data` / `order`):**
```json
{
  "status": "success",
  "data": {
    "order_id": "ORD900421",
    "order_status": "PENDING_PAYMENT",
    "payment_mode": "ONLINE",
    "payment_status": "PENDING",
    "total_amount": 960,
    "payment_link": "https://yourmall.com/pay/ORD900421",
    "payment_qr_url": "https://cdn.yourmall.com/qr/ORD900421.png",
    "payment_instructions": "Pay securely via UPI/Card using the link or QR above.",
    "message": "Your order #ORD900421 has been created."
  }
}
```
> For `COD`, `payment_link` and `payment_qr_url` can be omitted. When Central Brain finishes confirming COD or verifying Online payment, Central Brain MUST call `POST {bot_public_url}/api/sync/order` (`event: "order.confirmed"` or `"order.paid"`) so the bot clears the customer's cart and sends final confirmation + PDF receipt!

#### Case B: Async Order Cancellation (`event: "order.upserted"`)
Triggered only if a customer explicitly cancels an active order on WhatsApp:
```json
{
  "event": "order.upserted",
  "source": "whatsapp",
  "order": {
    "order_id": "ORD900421",
    "customer_phone": "919876543210",
    "order_status": "CANCELLED",
    "payment_status": "CANCELLED"
  },
  "timestamp": "2026-06-01T10:17:00Z"
}
```

---

### 4.4 Essential Async Business Events (Queued with 4x Exponential Backoff + 256-Item Retry Buffer)

All async events below are delivered through a 512-slot background worker (retrying at `1s → 2s → 4s → 8s`, then spilling into a 256-item retry buffer replayed on startup/manual sync & flushed before graceful shutdown). Central Brain should return any `2xx` JSON response.

#### 1. Customer Upsert — Change-Detected Only (`POST /api/vendor-bot/events/customer`)
Triggered **strictly** when a customer is brand-new OR when at least one profile field (`full_name`, `wa_number`, `contact_phone`, `delivery_address`, `latitude`, or `longitude`) changed compared to RAM. **Never** triggered on routine chats or internal state transitions.
```json
{
  "event": "customer.upserted",
  "source": "whatsapp",
  "customer": {
    "wa_number": "919876543210",
    "jid": "919876543210@s.whatsapp.net",
    "full_name": "Aarav Verma",
    "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
    "contact_phone": "919876543210",
    "latitude": 26.8512,
    "longitude": 80.9981,
    "source": "whatsapp"
  },
  "timestamp": "2026-06-01T10:11:00Z"
}
```

#### 2. Live Cart Sync (`POST /api/vendor-bot/events/cart`)
Triggered **only** when items are added, updated, removed, or cleared (from non-empty) on WhatsApp so the vendor's website & Mall cart stay in real-time sync.
```json
{
  "event": "cart.updated",
  "source": "whatsapp",
  "customer_phone": "919876543210",
  "jid": "919876543210@s.whatsapp.net",
  "items": [
    {
      "product_id": "p_101",
      "product_name": "Kaju Katli Royal Box",
      "unit_price": 480,
      "unit_type": "box",
      "qty": 2,
      "selected_variant": "500g",
      "line_total": 960,
      "source": "whatsapp"
    }
  ],
  "total_amount": 960,
  "timestamp": "2026-06-01T10:12:00Z"
}
```
*(When cart is cleared, `items` is `[]` and `total_amount` is `0`.)*

#### 3. On-Demand Magic Auto-Login Link (`POST /api/vendor-bot/events/magic-link`)
Triggered **only** when the customer explicitly asks for a website/mall link. Central Brain stores `token` (valid for 24h) and logs the customer into both the vendor website (`{store_url}?auth_token={token}`) and main Mall (`{mall_url}?auth_token={token}`) with their synced cart & address.
```json
{
  "event": "magic_link.created",
  "token": "9f8e7d6c5b4a3210fedcba9876543210abcdef1234567890",
  "jid": "919876543210@s.whatsapp.net",
  "customer_phone": "919876543210",
  "expires_at": "2026-06-02T10:12:00Z"
}
```

---

## 5. Inbound Webhooks: Live Push Updates (Central Brain → Vendor Bot)

Because the bot does **zero periodic polling**, Central Brain Node pushes all live business, catalog, website cart, website customer, payment, and order/delivery updates directly to `{bot_public_url}/api/sync/*`.

All `/api/sync/*` endpoints require `Authorization: Bearer <VENDOR_API_KEY>` or `X-Vendor-API-Key: <VENDOR_API_KEY>`. Invalid or missing auth returns `401 Unauthorized`.

### 5.1 Full RAM Hydration Push
- **Endpoint**: `POST {bot_public_url}/api/sync/full`
- **Payload**: Same object as `bootstrap.data` (`vendor`, `ai`, `payment`, `products`, `customers`, `active_carts`, `orders`, `whatsapp_session` — no `recent_chats`).
- **Bot Action**: Atomically replaces/merges RAM state.
- **Bot HTTP Response (`200 OK`)**:
```json
{
  "status": "success",
  "stats": {
    "products": 48,
    "customers": 120,
    "active_carts": 6,
    "orders": 84,
    "conversations": 35,
    "leads": 12,
    "media_cache_entries": 14
  }
}
```

---

### 5.2 Vendor Settings, Schedule, Admin Gates & AI Config Sync
- **Endpoint**: `POST {bot_public_url}/api/sync/vendor`
- **When to call**: Whenever vendor edits business details, open/holiday mode, delivery charges, AI model/prompt/keys, OR Central Admin toggles `admin_ai_auto_reply_allowed` / `subscription_active`.
- **Payload** *(include any subset of `vendor`, `ai`, `payment`)*:
```json
{
  "vendor": {
    "business_name": "Sharma Sweets & Mart",
    "is_open": false,
    "holiday_mode": true,
    "holiday_message": "Closed today for Diwali! Reopening tomorrow morning.",
    "operating_hours": "09:00 AM - 10:00 PM",
    "next_open_time": "Tomorrow 09:00 AM",
    "min_order_amount": 199,
    "estimated_delivery_time": "30-45 mins",
    "allow_cod": true,
    "subscription_active": true,
    "subscription_valid_until": "2027-01-01T00:00:00Z",
    "admin_ai_auto_reply_allowed": true,
    "admin_pause_reason": ""
  },
  "ai": {
    "provider": "groq",
    "api_key": "gsk_primary_key",
    "backup_api_keys": ["gsk_backup_1", "gsk_backup_2"],
    "model": "llama-3.3-70b-versatile",
    "temperature": 0.7,
    "system_prompt": "Always greet warmly and offer festive gift boxes.",
    "auto_reply": true,
    "import_catalog": true
  }
}
```
- **Bot HTTP Response (`200 OK`)**:
```json
{ "status": "success", "message": "Vendor configuration synced to RAM" }
```

---

### 5.3 Product Catalog, Variants, Stock & Image URL Sync
- **Endpoint**: `POST {bot_public_url}/api/sync/products`
- **Actions Supported**:
  - `"action": "replace"` + `"products": [...]` — replaces entire catalog in RAM.
  - `"action": "upsert"` + `"product": { ... }` (or `"products": [...]`) — adds/updates specific product(s) including live `stock_qty`, `in_stock`, `variants[]`, and full HTTPS `image_url` / `images[]`.
  - `"action": "delete"` + `"product_id": "p_101"` — removes product from RAM.

**Example (`upsert`):**
```json
{
  "action": "upsert",
  "product": {
    "id": "p_101",
    "name": "Kaju Katli Royal Box",
    "category": "Signature Sweets",
    "unit_type": "box",
    "price": 520,
    "sale_price": 480,
    "stock_qty": 18,
    "in_stock": true,
    "description": "Pure cashew diamond fudge made with silver leaf.",
    "image_url": "https://cdn.yourmall.com/kaju-katli-1.jpg",
    "images": [
      "https://cdn.yourmall.com/kaju-katli-1.jpg",
      "https://cdn.yourmall.com/kaju-katli-2.jpg"
    ],
    "variants": [
      { "name": "250g", "price": 245, "stock_qty": 10, "in_stock": true },
      { "name": "500g", "price": 480, "stock_qty": 8, "in_stock": true }
    ]
  }
}
```
- **Bot HTTP Response (`200 OK`)**:
```json
{ "status": "success", "product_count": 48 }
```

---

### 5.4 Website Customer Profile & Address Sync
- **Endpoint**: `POST {bot_public_url}/api/sync/customer`
- **When to call**: When a customer signs up or updates their delivery address on the Mall or Vendor website.
- **Payload** *(either wrapped in `"customer": {...}` or flat)*:
```json
{
  "customer": {
    "wa_number": "919876543210",
    "full_name": "Aarav Verma",
    "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
    "contact_phone": "919876543210",
    "latitude": 26.8512,
    "longitude": 80.9981,
    "source": "website"
  }
}
```
- **Bot HTTP Response (`200 OK`)**:
```json
{ "status": "success", "customer": { "...saved customer object..." } }
```

---

### 5.5 Website Cart Sync
- **Endpoint**: `POST {bot_public_url}/api/sync/cart`
- **When to call**: Whenever a customer adds, edits, or clears items on the website so the WhatsApp bot immediately knows their live website cart.
- **Payload**:
```json
{
  "customer_phone": "919876543210",
  "replace": true,
  "items": [
    {
      "product_id": "p_101",
      "product_name": "Kaju Katli Royal Box",
      "selected_variant": "500g",
      "unit_type": "box",
      "unit_price": 480,
      "qty": 2,
      "line_total": 960,
      "source": "website"
    }
  ]
}
```
*(Send `"items": []` with `"replace": true` to clear the customer's cart in RAM.)*
- **Bot HTTP Response (`200 OK`)**:
```json
{ "status": "success", "items": [ ... ], "total_amount": 960 }
```

---

### 5.6 Authoritative Order Confirmation, Payment Verification & Delivery Updates
- **Endpoint**: `POST {bot_public_url}/api/sync/order`
- **When to call**:
  1. **Final Order Confirmation / Payment Verified** (`event: "order.paid"`, `"order.confirmed"`, or `"order.created"`) — after Central Brain verifies an online payment, approves a COD WhatsApp order, or records a new website order.
     - **Bot Action**: Authoritatively updates order in RAM, updates customer state to `ORDER_CONFIRMED` in RAM, **clears active cart**, sends formatted WhatsApp confirmation text, and generates & delivers an **80mm Thermal PDF Receipt** (`Receipt_<order_id>.pdf`) directly from RAM!
  2. **Fulfillment & Delivery Partner Stages** (`event: "order.status_updated"` or `"order.delivery_assigned"`):
     - Supported `order_status` values (aliases normalized automatically):
       - `PROCESSING` (aliases: `PREPARING`, `PACKING`)
       - `PACKED` (aliases: `READY_FOR_PICKUP`, `READY`)
       - `DELIVERY_PARTNER_ASSIGNED` (aliases: `RIDER_ASSIGNED`, `PARTNER_ASSIGNED`, `ASSIGNED`)
       - `DISPATCHED` (aliases: `SHIPPED`)
       - `OUT_FOR_DELIVERY` (aliases: `ON_THE_WAY`, `PICKED_UP`)
       - `DELIVERED` (aliases: `COMPLETED`)
       - `CANCELLED` (aliases: `CANCELED`)
     - **Bot Action**: Updates RAM authoritatively (bot never lets stale local writes downgrade a Central-managed status rank) and sends a rich WhatsApp alert including `delivery_partner_name`, `delivery_partner_phone`, `delivery_otp`, and `estimated_delivery` whenever present.
     - Set `"notify_whatsapp": false` if you only want silent RAM hydration without sending a WhatsApp message.

**Example 1 — Final Payment Verification & Confirmation:**
```json
{
  "event": "order.paid",
  "notify_whatsapp": true,
  "order": {
    "order_id": "ORD900421",
    "customer_phone": "919876543210",
    "customer_name": "Aarav Verma",
    "delivery_phone": "919876543210",
    "delivery_address": "B-42 Gomti Nagar Extension, Lucknow",
    "items": [
      {
        "product_id": "p_101",
        "product_name": "Kaju Katli Royal Box",
        "selected_variant": "500g",
        "unit_price": 480,
        "qty": 2,
        "line_total": 960
      }
    ],
    "subtotal": 960,
    "delivery_fee": 0,
    "total_amount": 960,
    "payment_mode": "ONLINE",
    "payment_status": "PAID",
    "order_status": "CONFIRMED",
    "txn_id": "PAY_CENTRAL_99812",
    "estimated_delivery": "30-45 mins",
    "source": "whatsapp"
  }
}
```

**Example 2 — Delivery Partner Assigned with Rider Details & OTP:**
```json
{
  "event": "order.delivery_assigned",
  "notify_whatsapp": true,
  "order": {
    "order_id": "ORD900421",
    "customer_phone": "919876543210",
    "customer_name": "Aarav Verma",
    "total_amount": 960,
    "payment_mode": "ONLINE",
    "payment_status": "PAID",
    "order_status": "DELIVERY_PARTNER_ASSIGNED",
    "delivery_partner_name": "Rohit Kumar",
    "delivery_partner_phone": "+91 9811223344",
    "delivery_otp": "4829",
    "estimated_delivery": "20 mins"
  }
}
```

- **Bot HTTP Response (`200 OK`)**:
```json
{
  "status": "success",
  "status_changed": true,
  "order": { "...authoritative saved order object..." }
}
```

---

### 5.7 Direct WhatsApp Dispatch from Central Brain
- **Endpoint**: `POST {bot_public_url}/api/sync/send-message`
- **When to call**: When Central Brain wants to send any custom text or image notification through this vendor's connected WhatsApp number.
- **Payload**:
```json
{
  "phone": "919876543210",
  "message": "Your festive coupon DIWALI10 is now active!",
  "image_url": "https://cdn.yourmall.com/promo-banner.jpg"
}
```
*(Omit `image_url` to send plain text.)*
- **Bot HTTP Response**:
  - `200 OK`: `{ "status": "success", "message": "Delivered via vendor WhatsApp" }`
  - `503 Service Unavailable`: `{ "status": "error", "message": "Vendor WhatsApp is not connected" }`

---

### 5.8 Container Health Check
- **Endpoint**: `GET {bot_public_url}/api/health` *(public, no auth required — ideal for Render / Railway / Koyeb health probes)*
- **Bot HTTP Response (`200 OK`)**:
```json
{
  "status": "ok",
  "mode": "stateless-ram-bot",
  "database": "none",
  "sync_mode": "event-driven-zero-polling",
  "chat_sync": "ram-only-max-10",
  "bot_public_url": "https://vendor-shop-bot.onrender.com",
  "central_sync": true,
  "whatsapp": "Connected",
  "can_auto_reply": true,
  "auto_reply_pause_reason": "",
  "store_open": true,
  "store_closed_reason": "",
  "media_cache_entries": 14,
  "queue_depth": 0,
  "retry_buffer_depth": 0,
  "stats": {
    "products": 48,
    "customers": 120,
    "active_carts": 6,
    "orders": 84,
    "conversations": 35,
    "leads": 12,
    "media_cache_entries": 14
  },
  "timestamp": "2026-06-01T10:20:00Z"
}
```

---

## 6. Explicitly Removed Endpoints & Disabled Behaviors (`v2.3`)

To eliminate all unnecessary network load on Central Brain Node and maintain strict separation of concerns, the following endpoints and behaviors are **completely removed**:

- ❌ **`POST /api/vendor-bot/events/chat`** — Removed. Customer chats live strictly in bot RAM (last 10 messages per customer); zero chat messages are ever sent to Central Node.
- ❌ **`recent_chats` bootstrap field** — Removed. Bot never pulls historical customer chat transcripts from Central Node on startup.
- ❌ **`POST /api/vendor-bot/heartbeat` & periodic `SYNC_INTERVAL_SEC` polling** — Removed. Bot boots once via `/api/vendor-bot/bootstrap` and relies on Central Node's live `/api/sync/*` push webhooks thereafter (plus manual dashboard **Sync** button).
- ❌ **Per-message / debounced WhatsApp session uploads** — Removed. Session is uploaded/deleted strictly on first-time login, explicit logout/unlink, and graceful server shutdown.
- ❌ **`POST /api/vendor-bot/events/lead`** — Removed. Human escalations are saved in RAM and alerted directly to `vendor.notification_phone` via WhatsApp.
- ❌ **`/api/payment/verify-utr`** — Removed. Bot never verifies UTRs or payment screenshots.
- ❌ **`/api/remote/whatsapp/*`** — Removed. WhatsApp QR scan & 8-digit pairing exist **exclusively** on this Vendor Bot's own dashboard (`/api/whatsapp/status`, `/api/whatsapp/pair`, `/api/whatsapp/logout`).
