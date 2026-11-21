import express, { Request, Response, NextFunction } from 'express';
import cors from 'cors';
import crypto from 'crypto';
import path from 'path';
import { fileURLToPath } from 'url';
import QRCode from 'qrcode';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const app = express();
const PORT = Number(process.env.PORT || 3000);

app.use(cors());
app.use(express.json({ limit: '12mb' }));

// Return exact JSON decoding error reasons with HTTP 400 on malformed JSON payloads
app.use((err: any, _req: Request, res: Response, next: NextFunction) => {
  if (err && (err instanceof SyntaxError || err.type === 'entity.parse.failed') && 'body' in err) {
    res.status(400).json({
      status: 'error',
      message: `Invalid JSON body: ${err.message}`,
    });
    return;
  }
  next(err);
});

// =============================================================================
// 1. ZERO-DATABASE IN-MEMORY RAM STORE (Mirrors Go /ramstore/store.go v2.3)
// =============================================================================

interface VendorProfile {
  vendor_id: string;
  username: string;
  business_name: string;
  business_type: string;
  language: string;
  store_policies: string;
  store_slug: string;
  store_url: string;
  mall_url: string;
  notification_phone: string;
  latitude: number;
  longitude: number;
  max_delivery_radius_km: number;
  free_delivery_radius_km: number;
  delivery_charge_amount: number;
  require_customer_location: boolean;
  allow_cod: boolean;

  // Store Availability, Delivery Commitments & Central Admin Governance
  is_open: boolean;
  holiday_mode: boolean;
  holiday_message: string;
  operating_hours: string;
  next_open_time: string;
  min_order_amount: number;
  estimated_delivery_time: string;
  subscription_active: boolean;
  subscription_valid_until: string;
  admin_ai_auto_reply_allowed: boolean;
  admin_pause_reason: string;
}

interface AIConfig {
  provider: string;
  api_key: string;
  api_keys: string[];
  backup_api_keys: string[];
  model: string;
  temperature: number;
  system_prompt: string;
  auto_reply: boolean;
  import_catalog: boolean;
}

interface PaymentConfig {
  enabled: boolean;
  gateway: string;
  merchant_phone: string;
  merchant_vpa: string;
  paytm_mid: string;
  payee_name: string;
  cloudinary_cloud_name: string;
}

interface ProductVariant {
  id?: string;
  name: string;
  sku?: string;
  price?: number;
  sale_price?: number;
  mrp?: number;
  stock_qty?: number | null;
  in_stock?: boolean;
}

interface Product {
  id: string;
  name: string;
  price: number;
  sale_price: number;
  mrp: number;
  description: string;
  category: string;
  unit_type: string;
  variants?: ProductVariant[];
  stock_qty?: number | null;
  in_stock: boolean;
  image_url: string;
  thumb_url: string;
  images?: string[];
  whatsapp_catalog_link?: string;
}

interface Customer {
  wa_number: string;
  jid: string;
  full_name: string;
  delivery_address: string;
  contact_phone: string;
  latitude: number;
  longitude: number;
  last_order_details: string;
  current_state: string;
  payment_status: string;
  current_order_id: string;
  source: string;
  created_at: string;
  updated_at: string;
}

interface CartItem {
  product_id: string;
  product_name: string;
  unit_price: number;
  unit_type?: string;
  qty: number;
  selected_variant?: string;
  line_total: number;
  source: string;
  updated_at: string;
}

interface Order {
  order_id: string;
  customer_phone: string;
  jid: string;
  customer_name: string;
  delivery_phone: string;
  delivery_address: string;
  items: CartItem[];
  subtotal: number;
  delivery_fee: number;
  total_amount: number;
  payment_mode: string;
  payment_status: string;
  order_status: string;
  source: string;
  txn_id?: string;
  payment_link?: string;
  payment_qr_url?: string;
  delivery_partner_name?: string;
  delivery_partner_phone?: string;
  delivery_otp?: string;
  estimated_delivery?: string;
  status_updated_by?: string;
  created_at: string;
  updated_at: string;
}

interface ChatRecord {
  id: number;
  customer_phone: string;
  role: 'customer' | 'assistant';
  direction: string;
  text: string;
  time: string;
  created_at: string;
}

interface LeadRecord {
  id: string;
  customer_phone: string;
  inquiry_text: string;
  status: string;
  created_at: string;
}

interface WhatsAppSessionState {
  has_session: boolean;
  needs_login: boolean;
  status: 'Needs_Login' | 'QR_Ready' | 'Connecting' | 'Connected' | 'Disconnected';
  device_jid: string;
  phone: string;
  push_name: string;
  qr_code: string;
  qr_data_url?: string;
  last_synced_at?: string;
}

interface SyncStatus {
  central_url: string;
  bot_public_url: string;
  configured: boolean;
  connected: boolean;
  bootstrapped: boolean;
  last_bootstrap_at?: string;
  last_sync_at?: string;
  last_error?: string;
  outbound_pushed: number;
  inbound_received: number;
  queue_depth: number;
  retry_buffer_depth: number;
}

interface QueuedEvent {
  endpoint: string;
  payload: unknown;
  enqueued_at: string;
}

function normalizePhone(raw: string): string {
  const digits = String(raw || '').replace(/\D/g, '');
  if (!digits) return '';
  if (digits.length === 10 && /^[6-9]/.test(digits)) return '91' + digits;
  if (digits.length === 11 && digits.startsWith('0')) return '91' + digits.slice(1);
  return digits;
}

function isPlainObject(val: unknown): val is Record<string, any> {
  return Boolean(val) && typeof val === 'object' && !Array.isArray(val);
}

function firstDefined(obj: Record<string, any>, ...keys: string[]): any {
  for (const k of keys) {
    if (obj[k] !== undefined && obj[k] !== null) return obj[k];
  }
  return undefined;
}

function parseFlexString(val: unknown): string {
  if (val === undefined || val === null) return '';
  if (typeof val === 'string') return val.trim();
  if (typeof val === 'number') {
    if (!Number.isFinite(val)) return '';
    return Number.isInteger(val) ? String(val) : String(val);
  }
  if (typeof val === 'boolean') return val ? 'true' : 'false';
  return '';
}

function parseFlexNumber(val: unknown, fallback = 0): number {
  if (val === undefined || val === null || val === '') return fallback;
  if (typeof val === 'number') return Number.isFinite(val) ? val : fallback;
  if (typeof val === 'string') {
    const cleaned = val.trim().replace(/,/g, '');
    if (!cleaned) return fallback;
    const n = Number(cleaned);
    return Number.isFinite(n) ? n : fallback;
  }
  return fallback;
}

function parseFlexIntPtr(val: unknown): number | null {
  if (val === undefined || val === null || val === '') return null;
  const n = parseFlexNumber(val, Number.NaN);
  return Number.isFinite(n) ? Math.round(n) : null;
}

function parseFlexBoolOpt(val: unknown): boolean | undefined {
  if (val === undefined || val === null || val === '') return undefined;
  if (typeof val === 'boolean') return val;
  if (typeof val === 'number') return val !== 0;
  if (typeof val === 'string') {
    const s = val.trim().toLowerCase();
    if (['true', '1', 'yes', 'on', 'active', 'enabled'].includes(s)) return true;
    if (['false', '0', 'no', 'off', 'inactive', 'disabled'].includes(s)) return false;
  }
  return undefined;
}

function parseFlexBool(val: unknown, fallback: boolean): boolean {
  const parsed = parseFlexBoolOpt(val);
  return parsed !== undefined ? parsed : fallback;
}

function parseFlexStringArray(val: unknown): string[] {
  if (val === undefined || val === null) return [];
  if (Array.isArray(val)) {
    return val.map((item) => parseFlexString(item)).filter(Boolean);
  }
  if (typeof val === 'string') {
    const trimmed = val.trim();
    if (!trimmed) return [];
    if (trimmed.startsWith('[')) {
      try {
        const parsed = JSON.parse(trimmed);
        if (Array.isArray(parsed)) {
          return parsed.map((item) => parseFlexString(item)).filter(Boolean);
        }
      } catch {
        // Fallback to CSV/newline split below
      }
    }
    return trimmed
      .split(/[\n,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
  }
  return [];
}

// Accepts RFC3339, SQLite/SQL datetime ("YYYY-MM-DD HH:MM:SS" with optional fractional seconds/tz),
// date-only strings, or Unix epoch numbers/strings (seconds or ms).
function parseFlexibleTimestamp(raw: unknown): string {
  if (raw === undefined || raw === null) return '';
  if (typeof raw === 'number' && Number.isFinite(raw)) {
    if (raw === 0) return '';
    const ms = Math.abs(raw) >= 1e12 ? raw : raw * 1000;
    const d = new Date(ms);
    return Number.isNaN(d.getTime()) ? '' : d.toISOString();
  }
  const s = String(raw).trim();
  if (!s || s.toLowerCase() === 'null' || s === '0000-00-00 00:00:00') return '';

  if (/^\d{9,13}$/.test(s)) {
    const n = Number(s);
    const ms = s.length >= 12 ? n : n * 1000;
    const d = new Date(ms);
    return Number.isNaN(d.getTime()) ? '' : d.toISOString();
  }

  // Standard SQLite / SQL datetime: "YYYY-MM-DD HH:MM:SS" (+ optional fractional seconds / tz)
  const sqlMatch = s.match(/^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2}(?:\.\d+)?)(?:\s*(Z|[+-]\d{2}:?\d{2}))?$/i);
  if (sqlMatch) {
    const tz = sqlMatch[3] ? sqlMatch[3].toUpperCase() : 'Z';
    const d = new Date(`${sqlMatch[1]}T${sqlMatch[2]}${tz}`);
    if (!Number.isNaN(d.getTime())) return d.toISOString();
  }

  if (/^\d{4}-\d{2}-\d{2}$/.test(s)) {
    const d = new Date(`${s}T00:00:00Z`);
    if (!Number.isNaN(d.getTime())) return d.toISOString();
  }

  const parsed = Date.parse(s);
  if (!Number.isNaN(parsed)) return new Date(parsed).toISOString();
  return '';
}

function normalizeVariants(raw: any): ProductVariant[] | undefined {
  let list: any[] | null = null;
  if (Array.isArray(raw)) {
    list = raw;
  } else if (typeof raw === 'string') {
    const trimmed = raw.trim();
    if (trimmed.startsWith('[')) {
      try {
        const parsed = JSON.parse(trimmed);
        if (Array.isArray(parsed)) list = parsed;
      } catch {
        list = null;
      }
    } else if (trimmed) {
      list = trimmed
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    }
  }
  if (!list || list.length === 0) return undefined;

  const out: ProductVariant[] = [];
  for (const item of list) {
    if (typeof item === 'string' && item.trim()) {
      out.push({ name: item.trim(), in_stock: true });
    } else if (isPlainObject(item)) {
      const name = parseFlexString(firstDefined(item, 'name', 'label', 'title', 'variant', 'weight', 'size'));
      if (!name) continue;
      const stockQty = parseFlexIntPtr(firstDefined(item, 'stock_qty', 'stock', 'qty'));
      const inStockOpt = parseFlexBoolOpt(firstDefined(item, 'in_stock', 'available'));
      out.push({
        id: item.id !== undefined && item.id !== null ? parseFlexString(item.id) : undefined,
        name,
        sku: item.sku !== undefined && item.sku !== null ? parseFlexString(item.sku) : undefined,
        price: parseFlexNumber(item.price, 0),
        sale_price: parseFlexNumber(firstDefined(item, 'sale_price', 'offer_price'), 0),
        mrp: parseFlexNumber(item.mrp, 0),
        stock_qty: stockQty,
        in_stock: inStockOpt !== undefined ? inStockOpt : stockQty === null ? true : stockQty > 0,
      });
    }
  }
  return out.length > 0 ? out : undefined;
}

function decodeProductItem(raw: any, fallbackIndex?: number): Product {
  if (!isPlainObject(raw)) {
    throw new Error('product item is not a JSON object');
  }
  const id = parseFlexString(firstDefined(raw, 'id', 'product_id')) || (fallbackIndex !== undefined ? String(fallbackIndex + 1) : `prod_${Date.now()}`);
  const primaryImage = parseFlexString(
    firstDefined(raw, 'image_url', 'image', 'photo_url', 'img_url', 'product_image', 'primary_image')
  );
  const thumbUrl = parseFlexString(firstDefined(raw, 'thumb_url', 'thumbnail', 'thumbnail_url'));
  const rawGallery = parseFlexStringArray(firstDefined(raw, 'images', 'image_urls', 'gallery'));
  const images: string[] = [];
  const addImg = (u: string) => {
    const clean = u.trim();
    if (clean && !images.includes(clean)) images.push(clean);
  };
  addImg(primaryImage);
  for (const u of rawGallery) addImg(u);
  if (images.length === 0) addImg(thumbUrl);

  const stockQty = parseFlexIntPtr(firstDefined(raw, 'stock_qty', 'stock', 'quantity'));
  const inStockOpt = parseFlexBoolOpt(firstDefined(raw, 'in_stock', 'available', 'is_available'));
  const inStock = inStockOpt !== undefined ? inStockOpt : stockQty === null ? true : stockQty > 0;

  return {
    id,
    name: parseFlexString(firstDefined(raw, 'name', 'product_name', 'title')) || 'Item',
    price: parseFlexNumber(firstDefined(raw, 'price', 'unit_price'), 0),
    sale_price: parseFlexNumber(firstDefined(raw, 'sale_price', 'offer_price', 'discounted_price'), 0),
    mrp: parseFlexNumber(firstDefined(raw, 'mrp', 'compare_at_price'), 0),
    description: parseFlexString(firstDefined(raw, 'description', 'desc', 'short_description')),
    category: parseFlexString(firstDefined(raw, 'category', 'category_name')) || 'General',
    unit_type: parseFlexString(firstDefined(raw, 'unit_type', 'unit')) || 'pcs',
    variants: normalizeVariants(raw.variants),
    stock_qty: stockQty,
    in_stock: inStock,
    image_url: primaryImage || images[0] || thumbUrl || '',
    thumb_url: thumbUrl,
    images,
    whatsapp_catalog_link: parseFlexString(firstDefined(raw, 'whatsapp_catalog_link', 'catalog_link')) || undefined,
  };
}

function decodeCustomerItem(raw: any): Partial<Customer> {
  if (!isPlainObject(raw)) {
    throw new Error('customer item is not a JSON object');
  }
  return {
    wa_number: parseFlexString(firstDefined(raw, 'wa_number', 'customer_phone', 'phone', 'mobile', 'whatsapp_number')),
    jid: parseFlexString(raw.jid),
    full_name: parseFlexString(firstDefined(raw, 'full_name', 'customer_name', 'name')),
    delivery_address: parseFlexString(firstDefined(raw, 'delivery_address', 'address')),
    contact_phone: parseFlexString(firstDefined(raw, 'contact_phone', 'delivery_phone', 'alternate_phone')),
    latitude: parseFlexNumber(firstDefined(raw, 'latitude', 'lat'), 0),
    longitude: parseFlexNumber(firstDefined(raw, 'longitude', 'lng', 'lon'), 0),
    last_order_details: parseFlexString(raw.last_order_details),
    current_state: parseFlexString(firstDefined(raw, 'current_state', 'state')),
    payment_status: parseFlexString(raw.payment_status),
    current_order_id: raw.current_order_id !== undefined && raw.current_order_id !== null ? parseFlexString(raw.current_order_id) : undefined,
    source: parseFlexString(raw.source),
    created_at: parseFlexibleTimestamp(raw.created_at) || undefined,
    updated_at: parseFlexibleTimestamp(raw.updated_at) || undefined,
  };
}

function decodeCartItem(raw: any): Partial<CartItem> {
  if (!isPlainObject(raw)) {
    throw new Error('cart item is not a JSON object');
  }
  const product_id = parseFlexString(firstDefined(raw, 'product_id', 'id'));
  const product_name = parseFlexString(firstDefined(raw, 'product_name', 'name', 'title'));
  const unit_price = parseFlexNumber(firstDefined(raw, 'unit_price', 'price', 'sale_price'), 0);
  const qty = Math.round(parseFlexNumber(firstDefined(raw, 'qty', 'quantity'), 1));
  const rawTotal = parseFlexNumber(firstDefined(raw, 'line_total', 'total', 'subtotal'), 0);
  return {
    product_id,
    product_name,
    unit_price,
    unit_type: parseFlexString(firstDefined(raw, 'unit_type', 'unit')) || undefined,
    qty,
    selected_variant: parseFlexString(firstDefined(raw, 'selected_variant', 'variant', 'selectedVariant')) || undefined,
    line_total: rawTotal > 0 ? rawTotal : unit_price * Math.max(qty, 0),
    source: parseFlexString(raw.source) || undefined,
    updated_at: parseFlexibleTimestamp(firstDefined(raw, 'updated_at', 'created_at')) || undefined,
  };
}

function decodeCartItemsList(raw: any): { items: Partial<CartItem>[]; warnings: string[] } {
  if (raw === undefined || raw === null) return { items: [], warnings: [] };
  let arr: any = raw;
  if (typeof raw === 'string') {
    const trimmed = raw.trim();
    if (!trimmed || trimmed === 'null') return { items: [], warnings: [] };
    try {
      arr = JSON.parse(trimmed);
    } catch (err: any) {
      return { items: [], warnings: [`items JSON string invalid: ${err?.message || String(err)}`] };
    }
  }
  if (!Array.isArray(arr)) {
    return { items: [], warnings: ['items is not a valid array'] };
  }
  const items: Partial<CartItem>[] = [];
  const warnings: string[] = [];
  arr.forEach((it, idx) => {
    try {
      items.push(decodeCartItem(it));
    } catch (err: any) {
      warnings.push(`items[${idx}] skipped: ${err?.message || String(err)}`);
    }
  });
  return { items, warnings };
}

function decodeOrderItem(raw: any): Partial<Order> {
  if (!isPlainObject(raw)) {
    throw new Error('order item is not a JSON object');
  }
  const orderId = parseFlexString(firstDefined(raw, 'order_id', 'id'));
  if (!orderId) {
    throw new Error('order_id is missing or empty');
  }
  const { items: rawItems } = decodeCartItemsList(firstDefined(raw, 'items', 'order_items'));
  const items: CartItem[] = rawItems.map((it) => {
    const qty = Number(it.qty ?? 1);
    const unitPrice = Number(it.unit_price ?? 0);
    return {
      product_id: String(it.product_id || ''),
      product_name: String(it.product_name || (it.product_id ? `Product #${it.product_id}` : 'Item')),
      unit_price: unitPrice,
      unit_type: it.unit_type || 'pcs',
      qty,
      selected_variant: it.selected_variant,
      line_total: Number(it.line_total ?? unitPrice * qty),
      source: it.source || 'website',
      updated_at: it.updated_at || new Date().toISOString(),
    };
  });

  return {
    order_id: orderId,
    customer_phone: parseFlexString(firstDefined(raw, 'customer_phone', 'wa_number', 'phone')),
    jid: parseFlexString(raw.jid),
    customer_name: parseFlexString(firstDefined(raw, 'customer_name', 'full_name', 'name')),
    delivery_phone: parseFlexString(firstDefined(raw, 'delivery_phone', 'contact_phone')),
    delivery_address: parseFlexString(firstDefined(raw, 'delivery_address', 'address')),
    items,
    subtotal: parseFlexNumber(firstDefined(raw, 'subtotal', 'sub_total'), 0),
    delivery_fee: parseFlexNumber(firstDefined(raw, 'delivery_fee', 'delivery_charge'), 0),
    total_amount: parseFlexNumber(firstDefined(raw, 'total_amount', 'total', 'amount', 'grand_total'), 0),
    payment_mode: parseFlexString(firstDefined(raw, 'payment_mode', 'payment_method')),
    payment_status: parseFlexString(raw.payment_status),
    order_status: parseFlexString(firstDefined(raw, 'order_status', 'status')),
    source: parseFlexString(raw.source),
    txn_id: parseFlexString(firstDefined(raw, 'txn_id', 'transaction_id', 'payment_id')) || undefined,
    payment_link: parseFlexString(raw.payment_link) || undefined,
    payment_qr_url: parseFlexString(raw.payment_qr_url) || undefined,
    delivery_partner_name: parseFlexString(firstDefined(raw, 'delivery_partner_name', 'rider_name')) || undefined,
    delivery_partner_phone: parseFlexString(firstDefined(raw, 'delivery_partner_phone', 'rider_phone')) || undefined,
    delivery_otp: parseFlexString(firstDefined(raw, 'delivery_otp', 'otp')) || undefined,
    estimated_delivery: parseFlexString(firstDefined(raw, 'estimated_delivery', 'eta')) || undefined,
    status_updated_by: parseFlexString(raw.status_updated_by) || undefined,
    created_at: parseFlexibleTimestamp(raw.created_at) || undefined,
    updated_at: parseFlexibleTimestamp(raw.updated_at) || undefined,
  };
}

function effectivePrice(p: Product, variant?: ProductVariant): number {
  if (variant) {
    if (variant.sale_price && variant.sale_price > 0) return variant.sale_price;
    if (variant.price && variant.price > 0) return variant.price;
    if (variant.mrp && variant.mrp > 0) return variant.mrp;
  }
  if (p.sale_price > 0) return p.sale_price;
  if (p.price > 0) return p.price;
  return p.mrp || 0;
}

function isVariantInStock(v: ProductVariant): boolean {
  if (v.in_stock === false) return false;
  if (v.stock_qty !== undefined && v.stock_qty !== null && v.stock_qty <= 0) return false;
  return true;
}

function isProductInStock(p: Product): boolean {
  if (!p.in_stock) return false;
  if (p.variants && p.variants.length > 0) {
    return p.variants.some((v) => isVariantInStock(v));
  }
  if (p.stock_qty !== undefined && p.stock_qty !== null && p.stock_qty <= 0) return false;
  return true;
}

function resolveVariantAndStock(
  p: Product,
  requestedVariant: string | undefined,
  requestedQty: number
): { ok: boolean; variantName: string; price: number; reason?: string } {
  if (!p.in_stock) {
    return { ok: false, variantName: requestedVariant || '', price: effectivePrice(p), reason: `${p.name} abhi out of stock hai` };
  }
  if (p.variants && p.variants.length > 0) {
    const reqLower = String(requestedVariant || '').trim().toLowerCase();
    let chosen: ProductVariant | undefined;
    if (reqLower) {
      chosen = p.variants.find(
        (v) => v.name.toLowerCase() === reqLower || v.name.toLowerCase().includes(reqLower)
      );
    }
    if (!chosen) {
      chosen = p.variants.find((v) => isVariantInStock(v));
    }
    if (!chosen || !isVariantInStock(chosen)) {
      return {
        ok: false,
        variantName: requestedVariant || '',
        price: effectivePrice(p),
        reason: `${p.name}${requestedVariant ? ` (${requestedVariant})` : ''} abhi out of stock hai`,
      };
    }
    if (chosen.stock_qty !== undefined && chosen.stock_qty !== null && requestedQty > chosen.stock_qty) {
      return {
        ok: false,
        variantName: chosen.name,
        price: effectivePrice(p, chosen),
        reason: `${p.name} (${chosen.name}) ke sirf ${chosen.stock_qty} units stock me hain`,
      };
    }
    return { ok: true, variantName: chosen.name, price: effectivePrice(p, chosen) };
  }

  if (p.stock_qty !== undefined && p.stock_qty !== null) {
    if (p.stock_qty <= 0) {
      return { ok: false, variantName: '', price: effectivePrice(p), reason: `${p.name} abhi out of stock hai` };
    }
    if (requestedQty > p.stock_qty) {
      return { ok: false, variantName: '', price: effectivePrice(p), reason: `${p.name} ke sirf ${p.stock_qty} units stock me hain` };
    }
  }
  return { ok: true, variantName: requestedVariant || '', price: effectivePrice(p) };
}

function nowTimeHM(): string {
  const d = new Date();
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

function allAIKeys(): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const pushKey = (k?: string) => {
    const clean = String(k || '').trim();
    if (clean && !seen.has(clean)) {
      seen.add(clean);
      out.push(clean);
    }
  };
  pushKey(ramStore.ai.api_key);
  for (const k of ramStore.ai.api_keys || []) pushKey(k);
  for (const k of ramStore.ai.backup_api_keys || []) pushKey(k);
  return out;
}

function isSubscriptionValid(): { valid: boolean; reason: string } {
  const v = ramStore.vendor;
  if (v.subscription_active === false) {
    return { valid: false, reason: v.admin_pause_reason || 'Vendor subscription inactive' };
  }
  if (v.subscription_valid_until) {
    const exp = Date.parse(v.subscription_valid_until);
    if (!Number.isNaN(exp) && Date.now() > exp) {
      return { valid: false, reason: `Subscription expired on ${v.subscription_valid_until.slice(0, 10)}` };
    }
  }
  return { valid: true, reason: '' };
}

function isStoreOpenNow(): { open: boolean; reason: string } {
  const v = ramStore.vendor;
  if (v.holiday_mode) {
    return {
      open: false,
      reason: v.holiday_message || `Store is on holiday. Reopens: ${v.next_open_time || v.operating_hours || 'soon'}`,
    };
  }
  if (v.is_open === false) {
    return {
      open: false,
      reason: `Store is currently closed. Timings: ${v.operating_hours || '09:00 AM - 09:00 PM'}${v.next_open_time ? ` | Reopens: ${v.next_open_time}` : ''}`,
    };
  }
  return { open: true, reason: '' };
}

function canAutoReply(): { allowed: boolean; reason: string } {
  const v = ramStore.vendor;
  if (v.admin_ai_auto_reply_allowed === false) {
    return { allowed: false, reason: v.admin_pause_reason || 'AI Auto-Reply paused by Central Mall Admin' };
  }
  const sub = isSubscriptionValid();
  if (!sub.valid) {
    return { allowed: false, reason: sub.reason };
  }
  if (!ramStore.ai.auto_reply) {
    return { allowed: false, reason: 'Vendor disabled AI Auto-Reply' };
  }
  return { allowed: true, reason: '' };
}

// Runtime Central Brain Node configuration (Platform env auto-detection + request host fallback)
let centralNodeURL = (process.env.CENTRAL_NODE_URL || '').trim().replace(/\/+$/, '');
let vendorApiKey = (process.env.VENDOR_API_KEY || '').trim();
let botPublicURL = (
  process.env.BOT_PUBLIC_URL ||
  process.env.RENDER_EXTERNAL_URL ||
  (process.env.RAILWAY_PUBLIC_DOMAIN ? `https://${process.env.RAILWAY_PUBLIC_DOMAIN}` : '') ||
  (process.env.KOYEB_PUBLIC_DOMAIN ? `https://${process.env.KOYEB_PUBLIC_DOMAIN}` : '') ||
  (process.env.FLY_APP_NAME ? `https://${process.env.FLY_APP_NAME}.fly.dev` : '') ||
  (process.env.APP_URL ? process.env.APP_URL : '')
)
  .trim()
  .replace(/\/+$/, '');

const MAX_RETRY_BUFFER = 256;
const retryBuffer: QueuedEvent[] = [];
const mediaThumbCache = new Map<string, { url: string; cached_at: string; size_kb: number }>();

const ramStore = {
  vendor: {
    vendor_id: '',
    username: '',
    business_name: '',
    business_type: '',
    language: '',
    store_policies: '',
    store_slug: '',
    store_url: '',
    mall_url: '',
    notification_phone: '',
    latitude: 0,
    longitude: 0,
    max_delivery_radius_km: 0,
    free_delivery_radius_km: 0,
    delivery_charge_amount: 0,
    require_customer_location: false,
    allow_cod: true,

    is_open: true,
    holiday_mode: false,
    holiday_message: '',
    operating_hours: '',
    next_open_time: '',
    min_order_amount: 0,
    estimated_delivery_time: '',
    subscription_active: true,
    subscription_valid_until: '',
    admin_ai_auto_reply_allowed: true,
    admin_pause_reason: '',
  } as VendorProfile,

  ai: {
    provider: '',
    api_key: process.env.GROQ_API_KEY || process.env.GEMINI_API_KEY || process.env.OPENAI_API_KEY || '',
    api_keys: [],
    backup_api_keys: [],
    model: '',
    temperature: 0.7,
    system_prompt: '',
    auto_reply: true,
    import_catalog: true,
  } as AIConfig,

  payment: {
    enabled: false,
    gateway: '',
    merchant_phone: '',
    merchant_vpa: '',
    paytm_mid: '',
    payee_name: '',
    cloudinary_cloud_name: '',
  } as PaymentConfig,

  products: new Map<string, Product>(),
  customers: new Map<string, Customer>(),
  activeCarts: new Map<string, CartItem[]>(),
  orders: new Map<string, Order>(),
  chats: new Map<string, ChatRecord[]>(),
  leads: [] as LeadRecord[],
  chatSeq: 0,

  whatsapp: {
    has_session: false,
    needs_login: true,
    status: 'Needs_Login',
    device_jid: '',
    phone: '',
    push_name: '',
    qr_code: '',
  } as WhatsAppSessionState,

  sync: {
    central_url: centralNodeURL,
    bot_public_url: botPublicURL,
    configured: Boolean(centralNodeURL && vendorApiKey),
    connected: false,
    bootstrapped: false,
    outbound_pushed: 0,
    inbound_received: 0,
    queue_depth: 0,
    retry_buffer_depth: 0,
  } as SyncStatus,
};

// Auto-detect Public URL from incoming reverse-proxy host if env var wasn't set
app.use((req: Request, _res: Response, next: NextFunction) => {
  if (!botPublicURL) {
    const rawHost = String(req.headers['x-forwarded-host'] || req.headers.host || '').split(',')[0].trim();
    if (rawHost && !rawHost.startsWith('localhost') && !rawHost.startsWith('127.0.0.1') && !rawHost.startsWith('0.0.0.0')) {
      const proto = String(req.headers['x-forwarded-proto'] || 'https').split(',')[0].trim() || 'https';
      botPublicURL = `${proto}://${rawHost}`.replace(/\/+$/, '');
      ramStore.sync.bot_public_url = botPublicURL;
    }
  }
  next();
});

// Live SSE clients
const sseClients = new Set<Response>();

function pushLiveEvent(phone: string, text: string, type: string): void {
  const payload = JSON.stringify({
    phone: normalizePhone(phone),
    text,
    type,
    time: nowTimeHM(),
  });
  for (const res of sseClients) {
    try {
      res.write(`data: ${payload}\n\n`);
    } catch {
      sseClients.delete(res);
    }
  }
}

function appendChatRAM(phone: string, role: 'customer' | 'assistant', direction: string, text: string): ChatRecord | null {
  const cleanPhone = normalizePhone(phone);
  const cleanText = String(text || '').trim();
  if (!cleanPhone || !cleanText) return null;

  ramStore.chatSeq += 1;
  const rec: ChatRecord = {
    id: ramStore.chatSeq,
    customer_phone: cleanPhone,
    role,
    direction,
    text: cleanText,
    time: nowTimeHM(),
    created_at: new Date().toISOString(),
  };
  const list = ramStore.chats.get(cleanPhone) || [];
  list.push(rec);
  // Keep strictly the last 10 messages per customer in RAM (zero Central Node chat sync)
  if (list.length > 10) list.splice(0, list.length - 10);
  ramStore.chats.set(cleanPhone, list);
  return rec;
}

function findProductRAM(idOrName: string): Product | undefined {
  const clean = String(idOrName || '').trim();
  if (!clean) return undefined;
  if (ramStore.products.has(clean)) return ramStore.products.get(clean);
  const lower = clean.toLowerCase();
  for (const p of ramStore.products.values()) {
    if (p.name.toLowerCase() === lower || p.id.toLowerCase() === lower) return p;
  }
  for (const p of ramStore.products.values()) {
    if (p.name.toLowerCase().includes(lower)) return p;
  }
  return undefined;
}

function upsertCustomerWithChangeCheckRAM(partial: Partial<Customer>): { customer: Customer | null; changed: boolean } {
  const phone = normalizePhone(partial.wa_number || (partial.jid ? partial.jid.split('@')[0] : ''));
  if (!phone) return { customer: null, changed: false };
  const now = new Date().toISOString();
  const existing = ramStore.customers.get(phone);
  const incomingName = partial.full_name?.trim() || '';
  const incomingAddr = partial.delivery_address?.trim() || '';
  const incomingContact = normalizePhone(partial.contact_phone || '');
  const incomingLat = partial.latitude || 0;
  const incomingLng = partial.longitude || 0;

  let changed = !existing;
  if (existing) {
    if (incomingName && incomingName !== existing.full_name) changed = true;
    if (incomingAddr && incomingAddr !== existing.delivery_address) changed = true;
    if (incomingContact && incomingContact !== normalizePhone(existing.contact_phone)) changed = true;
    if (incomingLat !== 0 && incomingLat !== existing.latitude) changed = true;
    if (incomingLng !== 0 && incomingLng !== existing.longitude) changed = true;
  }

  const next: Customer = {
    wa_number: phone,
    jid: partial.jid || existing?.jid || `${phone}@s.whatsapp.net`,
    full_name: incomingName || existing?.full_name || '',
    delivery_address: incomingAddr || existing?.delivery_address || '',
    contact_phone: incomingContact || existing?.contact_phone || '',
    latitude: incomingLat || existing?.latitude || 0,
    longitude: incomingLng || existing?.longitude || 0,
    last_order_details: partial.last_order_details?.trim() || existing?.last_order_details || '',
    current_state: partial.current_state || existing?.current_state || 'chatting',
    payment_status: partial.payment_status || existing?.payment_status || 'UNPAID',
    current_order_id: partial.current_order_id !== undefined ? partial.current_order_id : existing?.current_order_id || '',
    source: partial.source || existing?.source || 'whatsapp',
    created_at: existing?.created_at || now,
    updated_at: now,
  };
  ramStore.customers.set(phone, next);
  return { customer: next, changed };
}

function upsertCustomerRAM(partial: Partial<Customer>): Customer | null {
  return upsertCustomerWithChangeCheckRAM(partial).customer;
}

function cartLineKey(productId: string, variant?: string): string {
  return `${productId}::${String(variant || '').trim().toLowerCase()}`;
}

function upsertCartValidatedRAM(
  phone: string,
  items: Partial<CartItem>[],
  replaceAll: boolean,
  enforceStock: boolean
): { items: CartItem[]; total: number; rejected: string[] } {
  const cleanPhone = normalizePhone(phone);
  if (!cleanPhone) return { items: [], total: 0, rejected: [] };

  const byKey = new Map<string, CartItem>();
  if (!replaceAll) {
    for (const ex of ramStore.activeCarts.get(cleanPhone) || []) {
      byKey.set(cartLineKey(ex.product_id, ex.selected_variant), ex);
    }
  }

  const rejected: string[] = [];
  const now = new Date().toISOString();

  for (const raw of items) {
    const prod = findProductRAM(raw.product_id || raw.product_name || '');
    const pid = prod?.id || String(raw.product_id || '').trim();
    if (!pid) continue;
    const qty = Number(raw.qty ?? 1);
    const key = cartLineKey(pid, raw.selected_variant);
    if (qty <= 0) {
      byKey.delete(key);
      continue;
    }

    let unitPrice = Number(raw.unit_price) > 0 ? Number(raw.unit_price) : prod ? effectivePrice(prod) : 0;
    let finalVariant = raw.selected_variant || '';

    if (prod && enforceStock) {
      const check = resolveVariantAndStock(prod, raw.selected_variant, qty);
      if (!check.ok) {
        rejected.push(check.reason || `${prod.name} out of stock`);
        continue;
      }
      finalVariant = check.variantName;
      if (Number(raw.unit_price) <= 0) unitPrice = check.price;
    }

    const name = raw.product_name?.trim() || prod?.name || `Product #${pid}`;
    byKey.set(cartLineKey(pid, finalVariant), {
      product_id: pid,
      product_name: name,
      unit_price: unitPrice,
      unit_type: raw.unit_type || prod?.unit_type || 'pcs',
      qty,
      selected_variant: finalVariant || undefined,
      line_total: unitPrice * qty,
      source: raw.source || 'whatsapp',
      updated_at: now,
    });
  }

  const finalItems = Array.from(byKey.values());
  const total = finalItems.reduce((sum, it) => sum + it.line_total, 0);
  if (finalItems.length === 0) {
    ramStore.activeCarts.delete(cleanPhone);
  } else {
    ramStore.activeCarts.set(cleanPhone, finalItems);
  }
  return { items: finalItems, total, rejected };
}

function upsertOrderRAM(raw: Partial<Order>, fromCentral = false): { order: Order | null; statusChanged: boolean } {
  const orderId = String(raw.order_id || '').trim();
  if (!orderId) return { order: null, statusChanged: false };
  const key = orderId.toUpperCase();
  const existing = ramStore.orders.get(key);
  const now = new Date().toISOString();
  const phone = normalizePhone(raw.customer_phone || existing?.customer_phone || '');

  // Central Brain Node is 100% authoritative for order confirmation, payment status, and delivery status
  const incomingStatus = raw.order_status ? String(raw.order_status).toUpperCase() : '';
  let nextStatus = incomingStatus || existing?.order_status || 'PENDING_CONFIRMATION';
  if (!fromCentral && existing && existing.status_updated_by === 'central') {
    nextStatus = existing.order_status;
  }

  const statusChanged = Boolean(
    fromCentral &&
      existing &&
      ((incomingStatus && incomingStatus !== existing.order_status) ||
        (raw.payment_status && raw.payment_status.toUpperCase() !== existing.payment_status) ||
        (raw.delivery_partner_name && raw.delivery_partner_name !== existing.delivery_partner_name))
  );

  const order: Order = {
    order_id: orderId,
    customer_phone: phone,
    jid: raw.jid || existing?.jid || (phone ? `${phone}@s.whatsapp.net` : ''),
    customer_name: raw.customer_name || existing?.customer_name || '',
    delivery_phone: normalizePhone(raw.delivery_phone || phone) || existing?.delivery_phone || '',
    delivery_address: raw.delivery_address || existing?.delivery_address || '',
    items: raw.items && raw.items.length > 0 ? raw.items : existing?.items || [],
    subtotal: Number(raw.subtotal ?? existing?.subtotal ?? 0),
    delivery_fee: Number(raw.delivery_fee ?? existing?.delivery_fee ?? 0),
    total_amount: Number(raw.total_amount ?? existing?.total_amount ?? 0),
    payment_mode: (raw.payment_mode || existing?.payment_mode || 'ONLINE').toUpperCase(),
    payment_status: (raw.payment_status || existing?.payment_status || 'PENDING').toUpperCase(),
    order_status: nextStatus,
    source: raw.source || existing?.source || (fromCentral ? 'website' : 'whatsapp'),
    txn_id: raw.txn_id || existing?.txn_id,
    payment_link: raw.payment_link || existing?.payment_link,
    payment_qr_url: raw.payment_qr_url || existing?.payment_qr_url,
    delivery_partner_name: raw.delivery_partner_name || existing?.delivery_partner_name,
    delivery_partner_phone: raw.delivery_partner_phone || existing?.delivery_partner_phone,
    delivery_otp: raw.delivery_otp || existing?.delivery_otp,
    estimated_delivery: raw.estimated_delivery || existing?.estimated_delivery,
    status_updated_by: fromCentral ? 'central' : existing?.status_updated_by || 'bot',
    created_at: existing?.created_at || raw.created_at || now,
    updated_at: now,
  };
  ramStore.orders.set(key, order);
  return { order, statusChanged };
}

function buildDeliveryStatusAlert(ord: Order): string {
  const st = ord.order_status.toUpperCase();
  const riderInfo = ord.delivery_partner_name
    ? `\n🛵 *Delivery Partner:* ${ord.delivery_partner_name}${ord.delivery_partner_phone ? ` (+${normalizePhone(ord.delivery_partner_phone)})` : ''}`
    : '';
  const otpInfo = ord.delivery_otp ? `\n🔐 *Delivery OTP:* *${ord.delivery_otp}* (Share only upon delivery)` : '';
  const etaInfo = ord.estimated_delivery ? `\n⏱️ *ETA:* ${ord.estimated_delivery}` : '';

  switch (st) {
    case 'CONFIRMED':
      return `🎉 *Order Confirmed!* (#${ord.order_id})\n\n• Total: *₹${ord.total_amount.toFixed(2)}* (${ord.payment_mode} · ${ord.payment_status})\n• 80mm Thermal PDF Receipt dispatched to customer on WhatsApp.${etaInfo}`;
    case 'PACKED':
      return `📦 *Order Packed!* (#${ord.order_id})\n\nAapka order pack ho kar ready ho gaya hai aur jald hi delivery partner ko handover kiya jayega.${etaInfo}`;
    case 'DELIVERY_PARTNER_ASSIGNED':
    case 'RIDER_ASSIGNED':
      return `🛵 *Delivery Partner Assigned!* (#${ord.order_id})${riderInfo}${otpInfo}${etaInfo}\n\nRider aapka order pickup karne store par pahunch raha hai.`;
    case 'OUT_FOR_DELIVERY':
    case 'DISPATCHED':
      return `🚀 *Out For Delivery!* (#${ord.order_id})${riderInfo}${otpInfo}${etaInfo}\n\nAapka order raste me hai! Kripya apna phone reachable rakhein.`;
    case 'DELIVERED':
      return `✅ *Order Delivered!* (#${ord.order_id})\n\n*${ramStore.vendor.business_name}* se shopping karne ke liye dhanyavaad! Agli baar fir seva ka mauka dein.`;
    case 'CANCELLED':
      return `❌ *Order Cancelled* (#${ord.order_id})\n\nKisi bhi sahayata ya refund jaankari ke liye yahan reply karein.`;
    default:
      return `📦 *Order #${ord.order_id} Update:* Status ab *${st}* hai.${riderInfo}${otpInfo}${etaInfo}`;
  }
}

function applyVendorSection(v: Record<string, any>): void {
  const curr = ramStore.vendor;
  const setStr = (val: any, prev: string) => (val !== undefined && val !== null ? parseFlexString(val) : prev);
  const setNum = (val: any, prev: number) => (val !== undefined && val !== null && val !== '' ? parseFlexNumber(val, prev) : prev);
  const setBool = (val: any, prev: boolean) => {
    const b = parseFlexBoolOpt(val);
    return b !== undefined ? b : prev;
  };

  ramStore.vendor = {
    vendor_id: setStr(firstDefined(v, 'vendor_id', 'id'), curr.vendor_id),
    username: setStr(firstDefined(v, 'username', 'user_name', 'handle'), curr.username),
    business_name: setStr(firstDefined(v, 'business_name', 'store_name', 'shop_name', 'name'), curr.business_name),
    business_type: setStr(firstDefined(v, 'business_type', 'category', 'store_type'), curr.business_type),
    language: setStr(firstDefined(v, 'language', 'response_language', 'bot_language'), curr.language),
    store_policies: setStr(firstDefined(v, 'store_policies', 'policies', 'policy'), curr.store_policies),
    store_slug: setStr(firstDefined(v, 'store_slug', 'slug'), curr.store_slug),
    store_url: setStr(firstDefined(v, 'store_url', 'website_url', 'shop_url'), curr.store_url),
    mall_url: setStr(firstDefined(v, 'mall_url', 'marketplace_url'), curr.mall_url),
    notification_phone: setStr(firstDefined(v, 'notification_phone', 'owner_phone', 'phone', 'whatsapp_phone'), curr.notification_phone),
    latitude: setNum(firstDefined(v, 'latitude', 'lat'), curr.latitude),
    longitude: setNum(firstDefined(v, 'longitude', 'lng', 'lon'), curr.longitude),
    max_delivery_radius_km: setNum(firstDefined(v, 'max_delivery_radius_km', 'delivery_radius_km'), curr.max_delivery_radius_km),
    free_delivery_radius_km: setNum(v.free_delivery_radius_km, curr.free_delivery_radius_km),
    delivery_charge_amount: setNum(firstDefined(v, 'delivery_charge_amount', 'delivery_charge', 'delivery_fee'), curr.delivery_charge_amount),
    require_customer_location: setBool(v.require_customer_location, curr.require_customer_location),
    allow_cod: setBool(firstDefined(v, 'allow_cod', 'cod_enabled'), curr.allow_cod),

    is_open: setBool(firstDefined(v, 'is_open', 'store_open'), curr.is_open),
    holiday_mode: setBool(firstDefined(v, 'holiday_mode', 'is_holiday'), curr.holiday_mode),
    holiday_message: setStr(v.holiday_message, curr.holiday_message),
    operating_hours: setStr(firstDefined(v, 'operating_hours', 'timings', 'store_hours'), curr.operating_hours),
    next_open_time: setStr(v.next_open_time, curr.next_open_time),
    min_order_amount: setNum(firstDefined(v, 'min_order_amount', 'minimum_order_amount', 'min_order'), curr.min_order_amount),
    estimated_delivery_time: setStr(firstDefined(v, 'estimated_delivery_time', 'delivery_eta', 'eta'), curr.estimated_delivery_time),
    subscription_active: setBool(v.subscription_active, curr.subscription_active),
    subscription_valid_until: setStr(firstDefined(v, 'subscription_valid_until', 'subscription_expires_at'), curr.subscription_valid_until),
    admin_ai_auto_reply_allowed: setBool(
      firstDefined(v, 'admin_ai_auto_reply_allowed', 'admin_ai_allowed'),
      curr.admin_ai_auto_reply_allowed
    ),
    admin_pause_reason: setStr(v.admin_pause_reason, curr.admin_pause_reason),
  };
}

function applyAISection(a: Record<string, any>): void {
  const curr = ramStore.ai;
  const tempRaw = firstDefined(a, 'temperature', 'ai_temperature');
  const nextTemp = tempRaw !== undefined && tempRaw !== null && tempRaw !== '' ? parseFlexNumber(tempRaw, curr.temperature) : curr.temperature;
  const keysRaw = a.api_keys;
  const backupRaw = firstDefined(a, 'backup_api_keys', 'fallback_api_keys');

  ramStore.ai = {
    provider: firstDefined(a, 'provider', 'ai_provider') !== undefined ? parseFlexString(firstDefined(a, 'provider', 'ai_provider')) : curr.provider,
    api_key: firstDefined(a, 'api_key', 'ai_api_key', 'primary_api_key') !== undefined
      ? parseFlexString(firstDefined(a, 'api_key', 'ai_api_key', 'primary_api_key'))
      : curr.api_key,
    api_keys: keysRaw !== undefined && keysRaw !== null ? parseFlexStringArray(keysRaw) : curr.api_keys,
    backup_api_keys: backupRaw !== undefined && backupRaw !== null ? parseFlexStringArray(backupRaw) : curr.backup_api_keys,
    model: firstDefined(a, 'model', 'ai_model') !== undefined ? parseFlexString(firstDefined(a, 'model', 'ai_model')) : curr.model,
    temperature: nextTemp > 0 ? nextTemp : 0.7,
    system_prompt: firstDefined(a, 'system_prompt', 'prompt', 'instructions') !== undefined
      ? parseFlexString(firstDefined(a, 'system_prompt', 'prompt', 'instructions'))
      : curr.system_prompt,
    auto_reply: parseFlexBool(firstDefined(a, 'auto_reply', 'ai_auto_reply', 'enabled'), curr.auto_reply),
    import_catalog: parseFlexBool(firstDefined(a, 'import_catalog', 'import_catalog_details'), curr.import_catalog),
  };
}

function applyPaymentSection(p: Record<string, any>): void {
  const curr = ramStore.payment;
  const gateway = firstDefined(p, 'gateway', 'provider', 'payment_gateway') !== undefined
    ? parseFlexString(firstDefined(p, 'gateway', 'provider', 'payment_gateway'))
    : curr.gateway;
  const merchantVpa = firstDefined(p, 'merchant_vpa', 'paytm_vpa', 'upi_id', 'vpa') !== undefined
    ? parseFlexString(firstDefined(p, 'merchant_vpa', 'paytm_vpa', 'upi_id', 'vpa'))
    : curr.merchant_vpa;
  const paytmMid = firstDefined(p, 'paytm_mid', 'mid', 'merchant_id') !== undefined
    ? parseFlexString(firstDefined(p, 'paytm_mid', 'mid', 'merchant_id'))
    : curr.paytm_mid;
  const payeeName = firstDefined(p, 'payee_name', 'merchant_name', 'upi_name') !== undefined
    ? parseFlexString(firstDefined(p, 'payee_name', 'merchant_name', 'upi_name'))
    : curr.payee_name || ramStore.vendor.business_name;
  const merchantPhone = firstDefined(p, 'merchant_phone', 'phone') !== undefined
    ? parseFlexString(firstDefined(p, 'merchant_phone', 'phone'))
    : curr.merchant_phone;
  const cloudinaryCloudName = p.cloudinary_cloud_name !== undefined && p.cloudinary_cloud_name !== null
    ? parseFlexString(p.cloudinary_cloud_name)
    : curr.cloudinary_cloud_name;
  const enabledOpt = parseFlexBoolOpt(firstDefined(p, 'enabled', 'online_payment_enabled', 'is_enabled', 'active'));
  const enabled = enabledOpt !== undefined ? enabledOpt : Boolean(gateway || merchantVpa || paytmMid || curr.enabled);

  ramStore.payment = {
    enabled,
    gateway,
    merchant_phone: merchantPhone,
    merchant_vpa: merchantVpa,
    paytm_mid: paytmMid,
    payee_name: payeeName,
    cloudinary_cloud_name: cloudinaryCloudName,
  };
}

// Resiliently hydrates every top-level section independently so a malformed item in one section
// (e.g., orders) NEVER blocks hydrating vendor, ai, payment, products, customers, active_carts, or whatsapp_session.
function hydrateRAM(payload: any): { warnings: string[] } {
  const warnings: string[] = [];
  if (!isPlainObject(payload)) {
    throw new Error('Request payload must be a JSON object');
  }

  // Unwrap {"status":"success","data":{...}} while keeping any top-level sibling keys
  const data: Record<string, any> = isPlainObject(payload.data)
    ? { ...payload, ...payload.data }
    : payload;

  // 1. Vendor / Business
  const vRaw = firstDefined(data, 'vendor', 'business', 'store');
  if (vRaw !== undefined) {
    try {
      if (!isPlainObject(vRaw)) throw new Error('vendor section is not a JSON object');
      applyVendorSection(vRaw);
    } catch (err: any) {
      warnings.push(`vendor: ${err?.message || String(err)}`);
    }
  }

  // 2. AI Config
  const aiRaw = firstDefined(data, 'ai', 'ai_config', 'ai_settings');
  if (aiRaw !== undefined) {
    try {
      if (!isPlainObject(aiRaw)) throw new Error('ai section is not a JSON object');
      applyAISection(aiRaw);
    } catch (err: any) {
      warnings.push(`ai: ${err?.message || String(err)}`);
    }
  }

  // 3. Payment Config
  const payRaw = firstDefined(data, 'payment', 'payment_config', 'payment_settings');
  if (payRaw !== undefined) {
    try {
      if (!isPlainObject(payRaw)) throw new Error('payment section is not a JSON object');
      applyPaymentSection(payRaw);
    } catch (err: any) {
      warnings.push(`payment: ${err?.message || String(err)}`);
    }
  }

  // 4. Products (item-by-item)
  const prodsRaw = firstDefined(data, 'products', 'catalog');
  if (prodsRaw !== undefined) {
    try {
      if (!Array.isArray(prodsRaw)) throw new Error('products section is not a JSON array');
      ramStore.products.clear();
      prodsRaw.forEach((rawP: any, idx: number) => {
        try {
          const p = decodeProductItem(rawP, idx);
          ramStore.products.set(p.id, p);
          if (p.image_url) {
            mediaThumbCache.set(p.image_url, { url: p.image_url, cached_at: new Date().toISOString(), size_kb: 14 });
          }
        } catch (err: any) {
          warnings.push(`products[${idx}] skipped: ${err?.message || String(err)}`);
        }
      });
    } catch (err: any) {
      warnings.push(`products: ${err?.message || String(err)}`);
    }
  }

  // 5. Customers (item-by-item)
  if (data.customers !== undefined && data.customers !== null) {
    try {
      if (!Array.isArray(data.customers)) throw new Error('customers section is not a JSON array');
      data.customers.forEach((rawC: any, idx: number) => {
        try {
          const c = decodeCustomerItem(rawC);
          upsertCustomerRAM(c);
        } catch (err: any) {
          warnings.push(`customers[${idx}] skipped: ${err?.message || String(err)}`);
        }
      });
    } catch (err: any) {
      warnings.push(`customers: ${err?.message || String(err)}`);
    }
  }

  // 6. Active Carts (supports both map {phone: items[]} and array [{customer_phone, items}])
  const cartsRaw = firstDefined(data, 'active_carts', 'carts');
  if (cartsRaw !== undefined) {
    try {
      if (Array.isArray(cartsRaw)) {
        cartsRaw.forEach((entry: any, idx: number) => {
          try {
            if (!isPlainObject(entry)) throw new Error('cart entry is not a JSON object');
            const phone = parseFlexString(firstDefined(entry, 'customer_phone', 'wa_number', 'phone'));
            if (!phone) return;
            const { items, warnings: w } = decodeCartItemsList(firstDefined(entry, 'items', 'cart_items'));
            w.forEach((msg) => warnings.push(`active_carts[${phone}]: ${msg}`));
            upsertCartValidatedRAM(phone, items, true, false);
          } catch (err: any) {
            warnings.push(`active_carts[${idx}] skipped: ${err?.message || String(err)}`);
          }
        });
      } else if (isPlainObject(cartsRaw)) {
        for (const [phone, itemsRaw] of Object.entries(cartsRaw)) {
          const { items, warnings: w } = decodeCartItemsList(itemsRaw);
          w.forEach((msg) => warnings.push(`active_carts[${phone}]: ${msg}`));
          upsertCartValidatedRAM(phone, items, true, false);
        }
      } else {
        throw new Error('active_carts section must be an object or array');
      }
    } catch (err: any) {
      warnings.push(`active_carts: ${err?.message || String(err)}`);
    }
  }

  // 7. Orders (item-by-item — malformed order NEVER blocks other orders or sections)
  if (data.orders !== undefined && data.orders !== null) {
    try {
      if (!Array.isArray(data.orders)) throw new Error('orders section is not a JSON array');
      data.orders.forEach((rawOrd: any, idx: number) => {
        try {
          const ord = decodeOrderItem(rawOrd);
          upsertOrderRAM(ord, true);
        } catch (err: any) {
          warnings.push(`orders[${idx}] skipped: ${err?.message || String(err)}`);
        }
      });
    } catch (err: any) {
      warnings.push(`orders: ${err?.message || String(err)}`);
    }
  }

  // 8. WhatsApp Session
  const wsRaw = firstDefined(data, 'whatsapp_session', 'whatsapp');
  if (wsRaw !== undefined) {
    try {
      if (!isPlainObject(wsRaw)) throw new Error('whatsapp_session section is not a JSON object');
      const blob = parseFlexString(firstDefined(wsRaw, 'session_blob_base64', 'session_blob'));
      const hasSession = parseFlexBool(wsRaw.has_session, Boolean(blob));
      const statusRaw = parseFlexString(wsRaw.status) as WhatsAppSessionState['status'];
      ramStore.whatsapp = {
        has_session: hasSession,
        needs_login: !hasSession,
        status: hasSession ? statusRaw || 'Connected' : 'Needs_Login',
        device_jid: parseFlexString(firstDefined(wsRaw, 'device_jid', 'jid')),
        phone: normalizePhone(parseFlexString(wsRaw.phone)),
        push_name: parseFlexString(wsRaw.push_name),
        qr_code: '',
        last_synced_at: parseFlexibleTimestamp(firstDefined(wsRaw, 'last_synced_at', 'updated_at')) || new Date().toISOString(),
      };
    } catch (err: any) {
      warnings.push(`whatsapp_session: ${err?.message || String(err)}`);
    }
  }

  const now = new Date().toISOString();
  ramStore.sync.bootstrapped = true;
  ramStore.sync.connected = true;
  ramStore.sync.last_bootstrap_at = now;
  ramStore.sync.last_sync_at = now;
  ramStore.sync.last_error = '';

  return { warnings };
}

function getStats() {
  return {
    products: ramStore.products.size,
    customers: ramStore.customers.size,
    active_carts: ramStore.activeCarts.size,
    orders: ramStore.orders.size,
    conversations: ramStore.chats.size,
    leads: ramStore.leads.length,
    media_cache_entries: mediaThumbCache.size,
  };
}

// =============================================================================
// 2. CENTRAL BRAIN NODE HTTP CLIENT (Bootstrap + Heartbeat + Retry Buffer)
// =============================================================================

async function callCentralNode(method: string, endpoint: string, body?: unknown): Promise<any> {
  if (!centralNodeURL || !vendorApiKey) {
    throw new Error('CENTRAL_NODE_URL and VENDOR_API_KEY are not configured');
  }
  const res = await fetch(`${centralNodeURL}${endpoint}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${vendorApiKey}`,
      'X-Vendor-API-Key': vendorApiKey,
      'User-Agent': 'VendorWhatsAppBotNode/2.2',
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(15000),
  });
  const text = await res.text();
  if (!res.ok) {
    throw new Error(`Central Node HTTP ${res.status}: ${text.slice(0, 200)}`);
  }
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

async function pushToCentralAsync(endpoint: string, payload: unknown): Promise<void> {
  if (!centralNodeURL || !vendorApiKey) return;
  ramStore.sync.queue_depth += 1;
  try {
    for (let attempt = 1; attempt <= 4; attempt++) {
      try {
        await callCentralNode('POST', endpoint, payload);
        ramStore.sync.outbound_pushed += 1;
        ramStore.sync.last_sync_at = new Date().toISOString();
        ramStore.sync.connected = true;
        ramStore.sync.last_error = '';
        return;
      } catch (err: any) {
        if (attempt === 4) {
          ramStore.sync.last_error = err?.message || String(err);
          if (retryBuffer.length >= MAX_RETRY_BUFFER) retryBuffer.shift();
          retryBuffer.push({ endpoint, payload, enqueued_at: new Date().toISOString() });
          ramStore.sync.retry_buffer_depth = retryBuffer.length;
        } else {
          await new Promise((r) => setTimeout(r, Math.pow(2, attempt - 1) * 1200));
        }
      }
    }
  } finally {
    ramStore.sync.queue_depth = Math.max(0, ramStore.sync.queue_depth - 1);
  }
}

async function replayRetryBuffer(): Promise<number> {
  if (!centralNodeURL || !vendorApiKey || retryBuffer.length === 0) return 0;
  let replayed = 0;
  const snapshot = retryBuffer.splice(0, retryBuffer.length);
  for (const item of snapshot) {
    try {
      await callCentralNode('POST', item.endpoint, item.payload);
      ramStore.sync.outbound_pushed += 1;
      replayed += 1;
    } catch {
      retryBuffer.push(item);
    }
  }
  ramStore.sync.retry_buffer_depth = retryBuffer.length;
  return replayed;
}

async function bootstrapFromCentral(): Promise<{ ok: boolean; error?: string }> {
  ramStore.sync.central_url = centralNodeURL;
  ramStore.sync.bot_public_url = botPublicURL;
  ramStore.sync.configured = Boolean(centralNodeURL && vendorApiKey);
  if (!ramStore.sync.configured) {
    ramStore.sync.connected = false;
    ramStore.sync.last_error = 'Set CENTRAL_NODE_URL & VENDOR_API_KEY to connect automatically on boot.';
    return { ok: false, error: ramStore.sync.last_error };
  }

  try {
    let data: any;
    try {
      data = await callCentralNode('POST', '/api/vendor-bot/bootstrap', {
        bot_public_url: botPublicURL,
        node_version: '2.3-zero-polling-zero-chat-sync',
        capabilities: [
          'auto_public_url',
          'zero_downtime_redeploy',
          'zero_polling_event_driven',
          'zero_chat_sync_ram_only',
          'admin_gating',
          'multi_key_ai_fallback',
          'variants_and_stock',
          'thumb_ram_cache',
          'debounce_and_voice',
          'delivery_partner_alerts',
          'central_controlled_checkout',
        ],
        timestamp: new Date().toISOString(),
      });
    } catch {
      data = await callCentralNode('GET', '/api/vendor-bot/bootstrap');
    }
    const { warnings } = hydrateRAM(data);
    if (warnings.length > 0) {
      console.warn(`⚠️ [Central Bootstrap] Non-fatal section warnings (${warnings.length}):`, warnings);
    }
    pushLiveEvent('', 'bootstrap', 'ram_synced');
    void replayRetryBuffer();
    console.log(`✅ [Central Bootstrap] Hydrated RAM:`, getStats());
    return { ok: true };
  } catch (err: any) {
    ramStore.sync.connected = false;
    ramStore.sync.last_error = err?.message || String(err);
    console.warn(`⚠️ [Central Bootstrap] ${ramStore.sync.last_error}`);
    return { ok: false, error: ramStore.sync.last_error };
  }
}

function verifyCentralWebhookAuth(req: Request, res: Response, next: NextFunction): void {
  if (!vendorApiKey) {
    next();
    return;
  }
  const headerKey = String(req.headers['x-vendor-api-key'] || '').trim();
  const authHeader = String(req.headers.authorization || '').trim();
  const bearerKey = authHeader.toLowerCase().startsWith('bearer ') ? authHeader.slice(7).trim() : '';
  const queryKey = String(req.query.api_key || '').trim();
  const provided = headerKey || bearerKey || queryKey;

  if (!provided || provided.length !== vendorApiKey.length || !crypto.timingSafeEqual(Buffer.from(provided), Buffer.from(vendorApiKey))) {
    res.status(401).json({ status: 'error', message: 'Unauthorized: invalid VENDOR_API_KEY' });
    return;
  }
  next();
}

// =============================================================================
// 3. CENTRAL-BRAIN-CONTROLLED ORDER SUBMISSION (COD & ONLINE PAYMENT)
//    Bot NEVER verifies payments locally, NEVER parses UTRs, NEVER marks PAID.
// =============================================================================

async function submitWhatsAppOrderToCentral(phone: string, requestedMode: string): Promise<{ reply: string; order?: Order }> {
  const cleanPhone = normalizePhone(phone);
  const items = ramStore.activeCarts.get(cleanPhone) || [];
  if (items.length === 0) {
    return { reply: `🛒 Aapka cart abhi khali hai. Kripya pehle kuch items add karein!` };
  }

  const subtotal = items.reduce((s, it) => s + it.line_total, 0);
  if (ramStore.vendor.min_order_amount > 0 && subtotal < ramStore.vendor.min_order_amount) {
    return {
      reply: `⚠️ *Minimum Order Amount:* ₹${ramStore.vendor.min_order_amount}\n\nAapka current cart *₹${subtotal.toFixed(2)}* ka hai — kripya ₹${(ramStore.vendor.min_order_amount - subtotal).toFixed(2)} ka aur item add karein!`,
    };
  }

  const cust = ramStore.customers.get(cleanPhone);
  const addr = cust?.delivery_address?.trim() || '';
  if (!addr) {
    return { reply: `📍 Order submit karne se pehle kripya apna complete *Delivery Address* share karein!` };
  }

  const mode = requestedMode.toUpperCase() === 'COD' ? 'COD' : 'ONLINE';
  if (mode === 'COD' && !ramStore.vendor.allow_cod) {
    return { reply: `ℹ️ Is store par abhi *Cash on Delivery (COD)* uplabdh nahi hai. Kripya *Online Payment* chunein!` };
  }

  const submission = {
    event: 'order.requested',
    source: 'whatsapp',
    customer_phone: cleanPhone,
    jid: `${cleanPhone}@s.whatsapp.net`,
    customer_name: cust?.full_name || 'WhatsApp Customer',
    delivery_phone: cust?.contact_phone || cleanPhone,
    delivery_address: addr,
    latitude: cust?.latitude || 0,
    longitude: cust?.longitude || 0,
    payment_mode: mode,
    items,
    subtotal,
    delivery_fee: 0,
    total_amount: subtotal,
    timestamp: new Date().toISOString(),
  };

  let centralOrderId = `ORD${Date.now()}`;
  let centralStatus = mode === 'ONLINE' ? 'PENDING_PAYMENT' : 'PENDING_CONFIRMATION';
  let centralPayStatus = mode === 'COD' ? 'COD_PENDING' : 'PENDING';
  let paymentLink = '';
  let paymentQrUrl = '';
  let paymentInstructions = '';
  let centralMsg = '';

  if (centralNodeURL && vendorApiKey) {
    try {
      const raw = await callCentralNode('POST', '/api/vendor-bot/events/order', submission);
      const data = raw?.data || raw?.order || raw || {};
      if (data.order_id) centralOrderId = String(data.order_id);
      if (data.order_status) centralStatus = String(data.order_status).toUpperCase();
      if (data.payment_status) centralPayStatus = String(data.payment_status).toUpperCase();
      if (data.payment_link) paymentLink = String(data.payment_link);
      if (data.payment_qr_url) paymentQrUrl = String(data.payment_qr_url);
      if (data.payment_instructions) paymentInstructions = String(data.payment_instructions);
      if (data.message) centralMsg = String(data.message);
      ramStore.sync.outbound_pushed += 1;
    } catch {
      void pushToCentralAsync('/api/vendor-bot/events/order', submission);
    }
  } else {
    // Preview mode demonstration when Central Node URL is not yet connected
    const mallBase = (ramStore.vendor.mall_url || 'https://supermall.in').replace(/\/+$/, '');
    if (mode === 'ONLINE') {
      paymentLink = `${mallBase}/pay/${centralOrderId}`;
      paymentInstructions = 'Complete online payment on Central Mall secure checkout page.';
    }
  }

  const { order: saved } = upsertOrderRAM(
    {
      order_id: centralOrderId,
      customer_phone: cleanPhone,
      customer_name: submission.customer_name,
      delivery_phone: submission.delivery_phone,
      delivery_address: submission.delivery_address,
      items,
      subtotal,
      delivery_fee: 0,
      total_amount: subtotal,
      payment_mode: mode,
      payment_status: centralPayStatus,
      order_status: centralStatus,
      payment_link: paymentLink,
      payment_qr_url: paymentQrUrl,
      estimated_delivery: ramStore.vendor.estimated_delivery_time,
      source: 'whatsapp',
    },
    true
  );

  upsertCustomerRAM({
    wa_number: cleanPhone,
    current_order_id: centralOrderId,
    current_state: mode === 'ONLINE' ? 'PAYMENT_PENDING' : 'ORDER_SUBMITTED',
    payment_status: centralPayStatus,
  });

  const parts: string[] = [];
  if (centralMsg) {
    parts.push(centralMsg);
  } else if (mode === 'ONLINE') {
    parts.push(
      `💳 *Order Reference #${centralOrderId} Submitted to Central System*\n\n• Payable Amount: *₹${subtotal.toFixed(2)}*\n• Payment Mode: *Online Payment*\n• Status: *${centralStatus}*`
    );
  } else {
    parts.push(
      `📦 *Order Request #${centralOrderId} Submitted to Central System*\n\n• Total Amount: *₹${subtotal.toFixed(2)}*\n• Payment Mode: *Cash on Delivery (COD)*\n• Status: *${centralStatus}*`
    );
  }
  if (paymentLink) parts.push(`🔗 *Central Payment Link:*\n👉 ${paymentLink}`);
  if (paymentInstructions) parts.push(`ℹ️ ${paymentInstructions}`);

  if (mode === 'ONLINE') {
    parts.push(
      `✅ Online payment aur verification 100% Central Brain System handle karta hai. Payment verify hote hi aapko final order confirmation aur PDF receipt turant WhatsApp par bhej di jayegi!`
    );
  } else {
    parts.push(
      `✅ Central Brain System se order confirm hote hi aapko final WhatsApp confirmation aur PDF receipt turant mil jayegi!`
    );
  }

  return { reply: parts.join('\n\n'), order: saved || undefined };
}

// =============================================================================
// 4. OCTOPUS AGENTIC AI ENGINE (Admin Gate + Store Hours + Multi-Key Fallback)
// =============================================================================

async function runOctopusAI(
  phone: string,
  query: string,
  overrideProvider?: string,
  overrideApiKey?: string,
  mediaType = 'text'
) {
  const cleanPhone = normalizePhone(phone) || '919999999999';

  // 1. Central Admin & Subscription Gate Check
  const gate = canAutoReply();
  if (!gate.allowed) {
    return {
      ai_reply_text: `⏸️ [Auto-Reply Paused] ${gate.reason}. (In production WhatsApp, the bot silently pauses AI replies.)`,
      actions: [{ hand: 'paused_by_gate', payload: { reason: gate.reason } }],
      engine: 'governance-gate',
      key_used_index: -1,
    };
  }

  // 2. Store Closed / Holiday Mode Check
  const storeOpen = isStoreOpenNow();
  if (!storeOpen.open) {
    const v = ramStore.vendor;
    const notice = v.holiday_mode
      ? `🏖️ *${v.business_name} — Holiday Notice*\n\n${storeOpen.reason}\n\nAap humari website par catalog browse kar sakte hain. Hum wapas khulte hi aapka order process karenge!`
      : `🌙 *${v.business_name} — Currently Closed*\n\n${storeOpen.reason}\n\nHum *${v.next_open_time || v.operating_hours}* par dobara khulenge. Aap apna order message chhod sakte hain!`;
    return {
      ai_reply_text: notice,
      actions: [{ hand: 'store_closed_notice', payload: { holiday_mode: v.holiday_mode, next_open_time: v.next_open_time } }],
      engine: 'store-hours-guard',
      key_used_index: -1,
    };
  }

  const candidateKeys: string[] = [];
  if (overrideApiKey?.trim()) candidateKeys.push(overrideApiKey.trim());
  for (const k of allAIKeys()) {
    if (!candidateKeys.includes(k)) candidateKeys.push(k);
  }

  const provider = (overrideProvider || ramStore.ai.provider || 'groq').toLowerCase();
  const inStockProducts = Array.from(ramStore.products.values()).filter((p) => isProductInStock(p));
  const currentCart = ramStore.activeCarts.get(cleanPhone) || [];
  const cust = ramStore.customers.get(cleanPhone);

  // 3. Multi-Key AI Fallback Loop (automatically rotates to next backup key on 429 / 5xx / timeout)
  if (candidateKeys.length > 0) {
    const inventoryText =
      inStockProducts.length > 0
        ? inStockProducts
            .map((p) => {
              const varSummary =
                p.variants && p.variants.length > 0
                  ? ` | Variants: ${p.variants
                      .filter((v) => isVariantInStock(v))
                      .map((v) => `${v.name} (Rs.${effectivePrice(p, v)}${v.stock_qty != null ? `, Stock:${v.stock_qty}` : ''})`)
                      .join(', ')}`
                  : p.stock_qty != null
                    ? ` | Stock: ${p.stock_qty} ${p.unit_type}`
                    : '';
              return `• ${p.name} (ID: ${p.id}, Cat: ${p.category}, Price: Rs.${effectivePrice(p)}/${p.unit_type}${varSummary})`;
            })
            .join('\n')
        : 'No in-stock products loaded in RAM yet.';

    const cartText =
      currentCart.length > 0
        ? currentCart
            .map((c) => `${c.product_name}${c.selected_variant ? ` [${c.selected_variant}]` : ''} x${c.qty} = Rs.${c.line_total}`)
            .join(', ')
        : 'Empty';

    const systemPrompt = `You are the automated WhatsApp sales & support agent for "${ramStore.vendor.business_name}" (${ramStore.vendor.business_type}).
Language: ${ramStore.vendor.language}. Timings: ${ramStore.vendor.operating_hours}. ETA: ${ramStore.vendor.estimated_delivery_time}. Min Order: Rs.${ramStore.vendor.min_order_amount}.
Store Policy: ${ramStore.vendor.store_policies}.
${ramStore.ai.system_prompt ? `Vendor Instructions: ${ramStore.ai.system_prompt}\n` : ''}NEVER suggest or add out-of-stock products/variants.
NEVER ask for UTR numbers or payment screenshots, NEVER scan/verify screenshots, and NEVER mark orders Paid locally.
When customer confirms checkout (COD or Online Payment), trigger {"hand": "submit_order", "payload": {"payment_mode": "COD" | "ONLINE"}} so Central Brain controls payment links, verification, and final confirmation.
Output ONLY raw JSON: {"ai_reply_text": "string", "actions": [{"hand": "update_cart|extract_profile|submit_order|generate_store_link|send_catalog_images|clear_cart|cancel_order|escalate_to_human|chat_only", "payload": {}}]}
LIVE IN-STOCK RAM INVENTORY:\n${inventoryText}
CURRENT CART: ${cartText}
CUSTOMER PROFILE: Name=${cust?.full_name || 'Unknown'}, Address=${cust?.delivery_address || 'Unknown'}, Phone=${cleanPhone}`;

    for (let idx = 0; idx < candidateKeys.length; idx++) {
      const apiKey = candidateKeys[idx];
      try {
        let rawText = '';
        if (provider === 'gemini' || apiKey.startsWith('AIzaSy')) {
          const model = ramStore.ai.model?.includes('gemini') ? ramStore.ai.model : 'gemini-2.0-flash';
          const r = await fetch(`https://generativelanguage.googleapis.com/v1beta/models/${model}:generateContent?key=${apiKey}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              system_instruction: { parts: [{ text: systemPrompt }] },
              contents: [{ role: 'user', parts: [{ text: query }] }],
              generationConfig: { temperature: ramStore.ai.temperature, response_mime_type: 'application/json' },
            }),
            signal: AbortSignal.timeout(10000),
          });
          if (!r.ok) throw new Error(`HTTP ${r.status}`);
          const json: any = await r.json();
          rawText = json?.candidates?.[0]?.content?.parts?.[0]?.text || '';
        } else {
          const endpoint =
            provider === 'openai' || apiKey.startsWith('sk-')
              ? 'https://api.openai.com/v1/chat/completions'
              : provider === 'deepseek'
                ? 'https://api.deepseek.com/chat/completions'
                : 'https://api.groq.com/openai/v1/chat/completions';
          const model =
            provider === 'openai' || apiKey.startsWith('sk-') ? 'gpt-4o-mini' : ramStore.ai.model || 'llama-3.3-70b-versatile';
          const r = await fetch(endpoint, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${apiKey}` },
            body: JSON.stringify({
              model,
              temperature: ramStore.ai.temperature,
              response_format: { type: 'json_object' },
              messages: [
                { role: 'system', content: systemPrompt },
                { role: 'user', content: query },
              ],
            }),
            signal: AbortSignal.timeout(10000),
          });
          if (!r.ok) throw new Error(`HTTP ${r.status}`);
          const json: any = await r.json();
          rawText = json?.choices?.[0]?.message?.content || '';
        }

        if (rawText) {
          const match = rawText.match(/\{[\s\S]*\}/);
          if (match) {
            const parsed = JSON.parse(match[0]);
            return {
              ai_reply_text: String(parsed.ai_reply_text || parsed.reply_to_customer || '').trim(),
              actions: Array.isArray(parsed.actions) ? parsed.actions : [],
              engine: `${provider} (key #${idx + 1}/${candidateKeys.length})`,
              key_used_index: idx,
            };
          }
        }
      } catch (err) {
        console.warn(`⚠️ [Multi-Key AI Fallback] Key #${idx + 1}/${candidateKeys.length} failed, rotating...`, err);
      }
    }
  }

  // 4. Deterministic RAM-Powered Octopus Responder (works offline / without API key)
  const lower = query.toLowerCase();
  const actions: Array<{ hand: string; payload?: any }> = [];
  let reply = '';

  if (lower.includes('website') || lower.includes('link') || lower.includes('storefront') || lower.includes('mall')) {
    const token = crypto.randomBytes(12).toString('hex');
    const mallBase = (ramStore.vendor.mall_url || centralNodeURL || 'https://supermall.in').replace(/\/+$/, '');
    const storeBase = (ramStore.vendor.store_url || `${mallBase}/${ramStore.vendor.store_slug || 'store'}`).replace(/\/+$/, '');
    const storeLink = `${storeBase}?auth_token=${token}`;
    const mallLink = `${mallBase}?auth_token=${token}`;
    actions.push({ hand: 'generate_store_link', payload: { store_url: storeLink, mall_url: mallLink } });
    void pushToCentralAsync('/api/vendor-bot/events/magic-link', {
      event: 'magic_link.created',
      token,
      customer_phone: cleanPhone,
      jid: `${cleanPhone}@s.whatsapp.net`,
      store_url: storeLink,
      mall_url: mallLink,
      expires_at: new Date(Date.now() + 86400000).toISOString(),
    });
    reply = `🛍️ *${ramStore.vendor.business_name} — Instant Auto-Login Links*\n\n🏪 *Direct Storefront:*\n👉 ${storeLink}\n\n🏬 *Full Multi-Vendor Mall:*\n👉 ${mallLink}\n\nAapka WhatsApp cart (variants sahit) aur delivery address dono jagah live synced rahega!`;
  } else if (lower.includes('photo') || lower.includes('image') || lower.includes('pic') || lower.includes('dikhao') && (lower.includes('photo') || lower.includes('image') || lower.includes('pic'))) {
    const matched =
      inStockProducts.find((p) => lower.includes(p.name.toLowerCase()) || p.name.toLowerCase().split(' ').some((w) => w.length >= 4 && lower.includes(w))) ||
      inStockProducts[0];
    if (matched) {
      const imgs = matched.images && matched.images.length > 0 ? matched.images : matched.image_url ? [matched.image_url] : [];
      imgs.forEach((u) => mediaThumbCache.set(u, { url: u, cached_at: new Date().toISOString(), size_kb: 12 }));
      const varStr =
        matched.variants && matched.variants.length > 0
          ? `\n🏷️ Variants: ${matched.variants
              .filter((v) => isVariantInStock(v))
              .map((v) => `${v.name} (₹${effectivePrice(matched, v)})`)
              .join(', ')}`
          : '';
      actions.push({
        hand: 'send_catalog_images',
        payload: { product_id: matched.id, image_url: imgs[0] || '', images_count: imgs.length, thumb_cache_hit: true },
      });
      reply = `📸 *[WhatsApp Product Photo + Standard JPEG Thumbnail Sent]*\n🔗 Image URL: ${imgs[0] || 'N/A'}\n\n🛍️ *${matched.name}*\n💰 Price: *₹${effectivePrice(matched).toFixed(2)}/${matched.unit_type}*${varStr}\n📝 ${matched.description || 'Fresh quality product from ' + ramStore.vendor.business_name}`;
    } else {
      reply = `Abhi RAM catalog me koi in-stock product photo uplabdh nahi hai.`;
    }
  } else if (lower.includes('malik') || lower.includes('owner') || lower.includes('human')) {
    actions.push({ hand: 'escalate_to_human', payload: {} });
    const lead: LeadRecord = {
      id: `lead_${Date.now()}`,
      customer_phone: cleanPhone,
      inquiry_text: query,
      status: 'Owner Escalation',
      created_at: new Date().toISOString(),
    };
    ramStore.leads.unshift(lead);
    reply = `🤝 *Store Owner Notified*\n\nMaine aapki request store owner ko bhej di hai. Woh jald hi aapse sampark karenge!`;
  } else if (lower.includes('address') || lower.includes('sector') || lower.includes('road') || lower.includes('nagar')) {
    actions.push({ hand: 'extract_profile', payload: { profile: { address: query } } });
    const total = currentCart.reduce((s, it) => s + it.line_total, 0);
    reply = `📍 Aapka delivery address RAM & Central Brain Node me save ho gaya hai!\n\n📋 *Order Summary*\n• Address: ${query}\n• Cart Total: ₹${total.toFixed(2)}\n• Est. Delivery: ${ramStore.vendor.estimated_delivery_time}\n\nCheckout ke liye batayein: *Online Payment* ya *Cash on Delivery (COD)*?`;
  } else if (lower.includes('cod') || lower.includes('cash on delivery')) {
    actions.push({ hand: 'submit_order', payload: { payment_mode: 'COD' } });
    reply = `Submitting your COD order request to Central Brain Node...`;
  } else if (lower.includes('online pay') || lower.includes('upi') || lower.includes('pay online') || lower.includes('payment link') || lower.includes('confirm order')) {
    actions.push({ hand: 'submit_order', payload: { payment_mode: 'ONLINE' } });
    reply = `Submitting your Online Payment order request to Central Brain Node...`;
  } else if (lower.includes('screenshot') || lower.includes('paid') || lower.includes('payment done')) {
    reply = `ℹ️ Online payments aur verification 100% humara Central Payment System automatic verify karta hai — screenshot ya UTR bhejne ki zaroorat nahi hai. Payment confirm hote hi aapko final order confirmation aur PDF receipt turant mil jayegi!`;
  } else {
    const allProds = Array.from(ramStore.products.values());
    const matchedAll = allProds.filter(
      (p) => lower.includes(p.name.toLowerCase()) || p.name.toLowerCase().split(' ').some((w) => w.length >= 4 && lower.includes(w))
    );
    const outOfStockMatched = matchedAll.filter((p) => !isProductInStock(p));
    const inStockMatched = matchedAll.filter((p) => isProductInStock(p));

    if (outOfStockMatched.length > 0 && inStockMatched.length === 0) {
      reply = `😔 Maaf kijiye! *${outOfStockMatched[0].name}* abhi *Out of Stock* hai, isliye cart me add nahi ho sakta. Aap humare dusre available items dekh sakte hain!`;
    } else if (
      inStockMatched.length > 0 &&
      (lower.includes('add') || lower.includes('order') || lower.includes('chahiye') || lower.includes('de do') || /\d/.test(lower) || mediaType === 'voice_note' || mediaType === 'burst')
    ) {
      const qtyMatch = lower.match(/\b([1-9])\b/);
      const qty = qtyMatch ? Number(qtyMatch[1]) : 1;
      const cartPayload = inStockMatched.slice(0, 3).map((p) => {
        let matchedVar = '';
        if (p.variants && p.variants.length > 0) {
          const foundVar = p.variants.find((v) => lower.includes(v.name.toLowerCase()));
          matchedVar = foundVar ? foundVar.name : p.variants.find((v) => isVariantInStock(v))?.name || '';
        }
        return { id: p.id, qty, variant: matchedVar };
      });
      actions.push({ hand: 'update_cart', payload: { cart_items: cartPayload } });
      const lines = cartPayload
        .map((ci) => {
          const p = ramStore.products.get(ci.id)!;
          const vObj = p.variants?.find((v) => v.name === ci.variant);
          const price = effectivePrice(p, vObj);
          return `• ${p.name}${ci.variant ? ` (${ci.variant})` : ''} x${ci.qty} — ₹${(price * ci.qty).toFixed(2)}`;
        })
        .join('\n');
      const prefix =
        mediaType === 'voice_note'
          ? `🎙️ *Voice Note Transcribed (Groq Whisper)*\n`
          : mediaType === 'burst'
            ? `⚡ *Debounced 3-Message Burst Combined (2.5s Window)*\n`
            : '';
      reply = `${prefix}🛒 *Cart Updated (RAM + Central Brain Synced)*\n\n${lines}\n\nCheckout ke liye apna delivery address bhejein, fir *Online Payment* ya *COD* choose karein!`;
    } else if (inStockProducts.length > 0) {
      const grouped = new Map<string, Product[]>();
      for (const p of inStockProducts) {
        const list = grouped.get(p.category) || [];
        list.push(p);
        grouped.set(p.category, list);
      }
      const catalogLines: string[] = [];
      for (const [cat, items] of grouped.entries()) {
        catalogLines.push(`📂 *${cat}:*`);
        items.slice(0, 5).forEach((p) => {
          const varTxt =
            p.variants && p.variants.length > 0
              ? ` [${p.variants
                  .filter((v) => isVariantInStock(v))
                  .map((v) => `${v.name}: ₹${effectivePrice(p, v)}`)
                  .join(' | ')}]`
              : ` — ₹${effectivePrice(p)}/${p.unit_type}`;
          catalogLines.push(`• ${p.name}${varTxt}`);
        });
      }
      reply = `✨ *Namaste! Welcome to ${ramStore.vendor.business_name}*\n⏱️ Delivery: ${ramStore.vendor.estimated_delivery_time} | Min Order: ₹${ramStore.vendor.min_order_amount}\n\nHumare live in-stock catalog me yeh items hain:\n\n${catalogLines.join('\n')}\n\nKaunsa item aur variant cart me add karein? (Kisi bhi item ki photo dekhne ke liye uska naam + 'photo' likhein!)`;
    } else {
      reply = `✨ *Namaste! Welcome to ${ramStore.vendor.business_name}*\n\nAbhi RAM catalog khali hai. Central Brain Node connect karein ya demo data load karein.`;
    }
  }

  return { ai_reply_text: reply, actions, engine: 'ram-octopus-v2.2', key_used_index: -1 };
}

// =============================================================================
// 5. ROUTES: HEALTH, OVERVIEW, GOVERNANCE, CENTRAL SYNC, RAM INSPECTOR
// =============================================================================

app.get('/api/health', (_req: Request, res: Response) => {
  const gate = canAutoReply();
  const storeOpen = isStoreOpenNow();
  res.json({
    status: 'ok',
    mode: 'stateless-ram-bot',
    database: 'none',
    sync_mode: 'event-driven-zero-polling',
    chat_sync: 'ram-only-max-10',
    checkout_authority: 'central-brain-node',
    whatsapp_login_scope: 'vendor-bot-dashboard-only',
    bot_public_url: botPublicURL,
    central_sync: ramStore.sync.connected,
    whatsapp: ramStore.whatsapp.status,
    can_auto_reply: gate.allowed,
    auto_reply_pause_reason: gate.reason,
    store_open: storeOpen.open,
    store_closed_reason: storeOpen.reason,
    media_cache_entries: mediaThumbCache.size,
    queue_depth: ramStore.sync.queue_depth,
    retry_buffer_depth: retryBuffer.length,
    stats: getStats(),
    timestamp: new Date().toISOString(),
  });
});

app.get('/api/bot/overview', (_req: Request, res: Response) => {
  const keys = allAIKeys();
  const primary = keys[0] || '';
  const maskedKey = primary.length > 8 ? `${primary.slice(0, 4)}••••${primary.slice(-4)}` : primary ? '••••' : '';
  const gate = canAutoReply();
  const storeOpen = isStoreOpenNow();
  const sub = isSubscriptionValid();

  res.json({
    status: 'success',
    sync: {
      ...ramStore.sync,
      bot_public_url: botPublicURL,
      retry_buffer_depth: retryBuffer.length,
    },
    vendor: ramStore.vendor,
    governance: {
      can_auto_reply: gate.allowed,
      auto_reply_pause_reason: gate.reason,
      store_open: storeOpen.open,
      store_closed_reason: storeOpen.reason,
      holiday_mode: ramStore.vendor.holiday_mode,
      operating_hours: ramStore.vendor.operating_hours,
      next_open_time: ramStore.vendor.next_open_time,
      min_order_amount: ramStore.vendor.min_order_amount,
      estimated_delivery_time: ramStore.vendor.estimated_delivery_time,
      subscription_valid: sub.valid,
      subscription_reason: sub.reason,
      subscription_valid_until: ramStore.vendor.subscription_valid_until,
      admin_ai_allowed: ramStore.vendor.admin_ai_auto_reply_allowed !== false,
      admin_pause_reason: ramStore.vendor.admin_pause_reason,
    },
    ai: {
      provider: ramStore.ai.provider,
      model: ramStore.ai.model,
      temperature: ramStore.ai.temperature,
      auto_reply: ramStore.ai.auto_reply,
      import_catalog: ramStore.ai.import_catalog,
      has_api_key: keys.length > 0,
      api_key_count: keys.length,
      backup_api_key_count: ramStore.ai.backup_api_keys.length,
      api_key_masked: maskedKey,
      system_prompt: ramStore.ai.system_prompt,
    },
    payment: {
      enabled:
        ramStore.payment.enabled ||
        Boolean(ramStore.payment.paytm_mid || ramStore.payment.merchant_vpa || ramStore.payment.gateway),
      gateway: ramStore.payment.gateway,
      merchant_vpa: ramStore.payment.merchant_vpa,
      paytm_mid: ramStore.payment.paytm_mid,
      payee_name: ramStore.payment.payee_name,
      merchant_phone: ramStore.payment.merchant_phone,
      cloudinary_cloud_name: ramStore.payment.cloudinary_cloud_name,
    },
    whatsapp: ramStore.whatsapp,
    media_cache_entries: mediaThumbCache.size,
    stats: getStats(),
  });
});

app.post('/api/bot/governance', (req: Request, res: Response) => {
  const b = req.body || {};
  if (typeof b.is_open === 'boolean') ramStore.vendor.is_open = b.is_open;
  if (typeof b.holiday_mode === 'boolean') ramStore.vendor.holiday_mode = b.holiday_mode;
  if (typeof b.holiday_message === 'string') ramStore.vendor.holiday_message = b.holiday_message;
  if (typeof b.operating_hours === 'string') ramStore.vendor.operating_hours = b.operating_hours;
  if (typeof b.next_open_time === 'string') ramStore.vendor.next_open_time = b.next_open_time;
  if (typeof b.min_order_amount === 'number') ramStore.vendor.min_order_amount = b.min_order_amount;
  if (typeof b.estimated_delivery_time === 'string') ramStore.vendor.estimated_delivery_time = b.estimated_delivery_time;
  if (typeof b.subscription_active === 'boolean') ramStore.vendor.subscription_active = b.subscription_active;
  if (typeof b.admin_ai_auto_reply_allowed === 'boolean') ramStore.vendor.admin_ai_auto_reply_allowed = b.admin_ai_auto_reply_allowed;
  if (typeof b.admin_pause_reason === 'string') ramStore.vendor.admin_pause_reason = b.admin_pause_reason;
  if (typeof b.auto_reply === 'boolean') ramStore.ai.auto_reply = b.auto_reply;

  pushLiveEvent('', 'governance', 'ram_synced');
  const gate = canAutoReply();
  const storeOpen = isStoreOpenNow();
  res.json({
    status: 'success',
    can_auto_reply: gate.allowed,
    auto_reply_pause_reason: gate.reason,
    store_open: storeOpen.open,
    store_closed_reason: storeOpen.reason,
  });
});

app.post('/api/bot/resync', async (_req: Request, res: Response) => {
  const result = await bootstrapFromCentral();
  if (!result.ok) {
    res.status(502).json({ status: 'error', message: result.error });
    return;
  }
  const replayed = await replayRetryBuffer();
  res.json({ status: 'success', message: `RAM reconciled from Central Brain Node (${replayed} queued events replayed).`, stats: getStats() });
});

app.post('/api/bot/connect-central', async (req: Request, res: Response) => {
  const { central_node_url, vendor_api_key, bot_public_url } = req.body || {};
  if (!central_node_url || !vendor_api_key) {
    res.status(400).json({ status: 'error', message: 'central_node_url and vendor_api_key are required' });
    return;
  }
  centralNodeURL = String(central_node_url).trim().replace(/\/+$/, '');
  vendorApiKey = String(vendor_api_key).trim();
  if (bot_public_url) {
    botPublicURL = String(bot_public_url).trim().replace(/\/+$/, '');
    ramStore.sync.bot_public_url = botPublicURL;
  }

  const result = await bootstrapFromCentral();
  if (!result.ok) {
    res.status(502).json({ status: 'error', message: `Credentials saved, but bootstrap failed: ${result.error}` });
    return;
  }
  res.json({ status: 'success', message: 'Connected to Central Brain Node and hydrated RAM.', stats: getStats() });
});

// RAM Inspector Endpoints
app.get('/api/ram/products', (_req, res) => {
  res.json({ status: 'success', products: Array.from(ramStore.products.values()) });
});
app.get('/api/ram/customers', (_req, res) => {
  const customers = Array.from(ramStore.customers.values()).sort((a, b) => b.updated_at.localeCompare(a.updated_at));
  res.json({ status: 'success', customers });
});
app.get('/api/ram/carts', (_req, res) => {
  const carts: Record<string, CartItem[]> = {};
  for (const [k, v] of ramStore.activeCarts.entries()) carts[k] = v;
  res.json({ status: 'success', carts });
});
app.get('/api/ram/orders', (_req, res) => {
  const orders = Array.from(ramStore.orders.values()).sort((a, b) => b.created_at.localeCompare(a.created_at));
  res.json({ status: 'success', orders });
});
app.get('/api/ram/leads', (_req, res) => {
  res.json({ status: 'success', leads: ramStore.leads });
});

// =============================================================================
// 6. INBOUND WEBHOOKS: CENTRAL BRAIN NODE -> VENDOR BOT (Website/Portal -> RAM -> WA)
// =============================================================================

function markInbound(section: string) {
  ramStore.sync.inbound_received += 1;
  ramStore.sync.last_sync_at = new Date().toISOString();
  ramStore.sync.connected = true;
  pushLiveEvent('', section, 'ram_synced');
}

app.post('/api/sync/full', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  try {
    const { warnings } = hydrateRAM(req.body);
    markInbound('full');
    const resp: Record<string, any> = { status: 'success', stats: getStats() };
    if (warnings.length > 0) resp.warnings = warnings;
    res.json(resp);
  } catch (err: any) {
    res.status(400).json({ status: 'error', message: `Invalid JSON body: ${err?.message || String(err)}` });
  }
});

app.post('/api/sync/vendor', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const data = isPlainObject(req.body.data) ? { ...req.body, ...req.body.data } : req.body;
  const warnings: string[] = [];

  const vRaw = firstDefined(data, 'vendor', 'business', 'store');
  if (vRaw !== undefined) {
    if (isPlainObject(vRaw)) applyVendorSection(vRaw);
    else warnings.push('vendor section is not a JSON object');
  }
  const aiRaw = firstDefined(data, 'ai', 'ai_config', 'ai_settings');
  if (aiRaw !== undefined) {
    if (isPlainObject(aiRaw)) applyAISection(aiRaw);
    else warnings.push('ai section is not a JSON object');
  }
  const payRaw = firstDefined(data, 'payment', 'payment_config', 'payment_settings');
  if (payRaw !== undefined) {
    if (isPlainObject(payRaw)) applyPaymentSection(payRaw);
    else warnings.push('payment section is not a JSON object');
  }

  markInbound('vendor');
  const resp: Record<string, any> = { status: 'success', message: 'Vendor, AI & Payment configuration synced to RAM' };
  if (warnings.length > 0) resp.warnings = warnings;
  res.json(resp);
});

app.post('/api/sync/products', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const { action, products, product } = req.body;
  const act = parseFlexString(action || 'replace').toLowerCase();
  const productId = parseFlexString(firstDefined(req.body, 'product_id', 'id'));
  const warnings: string[] = [];

  if (act === 'delete') {
    if (productId) ramStore.products.delete(productId);
  } else if (act === 'upsert') {
    const list = product !== undefined && product !== null ? [product] : Array.isArray(products) ? products : [];
    list.forEach((rawP: any, idx: number) => {
      try {
        const p = decodeProductItem(rawP, idx);
        ramStore.products.set(p.id, p);
        if (p.image_url) {
          mediaThumbCache.set(p.image_url, { url: p.image_url, cached_at: new Date().toISOString(), size_kb: 14 });
        }
      } catch (err: any) {
        warnings.push(`products[${idx}] skipped: ${err?.message || String(err)}`);
      }
    });
  } else if (products !== undefined && products !== null) {
    if (!Array.isArray(products)) {
      res.status(400).json({ status: 'error', message: 'Invalid JSON body: products must be a JSON array' });
      return;
    }
    ramStore.products.clear();
    products.forEach((rawP: any, idx: number) => {
      try {
        const p = decodeProductItem(rawP, idx);
        ramStore.products.set(p.id, p);
        if (p.image_url) {
          mediaThumbCache.set(p.image_url, { url: p.image_url, cached_at: new Date().toISOString(), size_kb: 14 });
        }
      } catch (err: any) {
        warnings.push(`products[${idx}] skipped: ${err?.message || String(err)}`);
      }
    });
  } else if (product !== undefined && product !== null) {
    try {
      const p = decodeProductItem(product);
      ramStore.products.set(p.id, p);
      if (p.image_url) {
        mediaThumbCache.set(p.image_url, { url: p.image_url, cached_at: new Date().toISOString(), size_kb: 14 });
      }
    } catch (err: any) {
      res.status(400).json({ status: 'error', message: `Invalid product JSON: ${err?.message || String(err)}` });
      return;
    }
  }

  markInbound('products');
  const resp: Record<string, any> = { status: 'success', product_count: ramStore.products.size };
  if (warnings.length > 0) resp.warnings = warnings;
  res.json(resp);
});

app.post('/api/sync/customer', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const targetRaw = req.body.customer !== undefined && req.body.customer !== null ? req.body.customer : req.body;
  try {
    const decoded = decodeCustomerItem(targetRaw);
    const saved = upsertCustomerRAM({ ...decoded, source: decoded.source || 'website' });
    if (!saved) {
      res.status(400).json({ status: 'error', message: 'Invalid customer JSON: wa_number / customer_phone is required' });
      return;
    }
    markInbound('customer');
    res.json({ status: 'success', customer: saved });
  } catch (err: any) {
    res.status(400).json({ status: 'error', message: `Invalid customer JSON: ${err?.message || String(err)}` });
  }
});

app.post('/api/sync/cart', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const phone = parseFlexString(firstDefined(req.body, 'customer_phone', 'phone', 'wa_number'));
  if (!phone) {
    res.status(400).json({ status: 'error', message: 'customer_phone is required' });
    return;
  }
  const replace = parseFlexBool(req.body.replace, true);
  const { items: decodedItems, warnings } = decodeCartItemsList(firstDefined(req.body, 'items', 'cart_items'));
  const items = decodedItems.map((it) => ({ ...it, source: it.source || 'website' }));
  const result = upsertCartValidatedRAM(phone, items, replace, false);
  markInbound('cart');
  const resp: Record<string, any> = { status: 'success', items: result.items, total_amount: result.total };
  if (warnings.length > 0) resp.warnings = warnings;
  res.json(resp);
});

// Central Brain pushes final order confirmation (after verifying online payment or approving COD) & delivery partner updates here.
app.post('/api/sync/order', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const rawOrder = req.body.order !== undefined && req.body.order !== null ? req.body.order : req.body;
  let decodedOrder: Partial<Order>;
  try {
    decodedOrder = decodeOrderItem(rawOrder);
  } catch (err: any) {
    res.status(400).json({ status: 'error', message: `Invalid order JSON: ${err?.message || String(err)}` });
    return;
  }
  const { order: saved, statusChanged } = upsertOrderRAM({ ...decodedOrder, source: decodedOrder.source || 'website' }, true);
  const ev = parseFlexString(req.body.event).toLowerCase();
  const isFinalConfirm =
    ev === 'order.confirmed' ||
    ev === 'order.paid' ||
    ev === 'order.created' ||
    (statusChanged && (saved?.order_status === 'CONFIRMED' || saved?.payment_status === 'PAID'));

  if (saved && saved.customer_phone) {
    upsertCustomerRAM({
      wa_number: saved.customer_phone,
      full_name: saved.customer_name,
      delivery_address: saved.delivery_address,
      contact_phone: saved.delivery_phone,
      current_state: isFinalConfirm ? 'ORDER_CONFIRMED' : undefined,
      payment_status: saved.payment_status,
      current_order_id: isFinalConfirm ? '' : saved.order_id,
      last_order_details: `Order #${saved.order_id} (₹${saved.total_amount.toFixed(2)} • ${saved.order_status})`,
      source: saved.source,
    });
    if (isFinalConfirm) {
      ramStore.activeCarts.delete(saved.customer_phone);
    }
    const notifyOpt = parseFlexBoolOpt(req.body.notify_whatsapp);
    const shouldNotify = notifyOpt !== undefined ? notifyOpt : statusChanged || isFinalConfirm;
    if (shouldNotify) {
      const msg = isFinalConfirm
        ? `🎉 *Order Confirmed by Central Mall!* (#${saved.order_id})\n\n• Total: *₹${saved.total_amount.toFixed(2)}* (${saved.payment_mode} · ${saved.payment_status})\n• 80mm Thermal PDF Receipt generated and sent to customer on WhatsApp!`
        : buildDeliveryStatusAlert(saved);
      appendChatRAM(saved.customer_phone, 'assistant', 'outgoing_ai', msg);
      pushLiveEvent(saved.customer_phone, msg, 'outgoing_ai');
    }
  }
  markInbound('order');
  res.json({ status: 'success', status_changed: statusChanged, order: saved });
});

app.post('/api/sync/send-message', verifyCentralWebhookAuth, (req: Request, res: Response) => {
  if (!isPlainObject(req.body)) {
    res.status(400).json({ status: 'error', message: 'Invalid JSON body: expected a JSON object' });
    return;
  }
  const phone = parseFlexString(req.body.phone);
  const message = parseFlexString(req.body.message);
  if (!phone || !message) {
    res.status(400).json({ status: 'error', message: 'phone and message are required' });
    return;
  }
  appendChatRAM(phone, 'assistant', 'outgoing_ai', message);
  pushLiveEvent(phone, message, 'outgoing_ai');
  markInbound('send_message');
  res.json({ status: 'success', message: 'Queued for WhatsApp delivery' });
});

// =============================================================================
// 7. VENDOR BOT DASHBOARD-ONLY WHATSAPP LOGIN (QR & 8-DIGIT PAIRING)
//    Central Brain saves/restores the WhatsMeow session automatically;
//    login UI & pairing endpoints exist ONLY on this Vendor Bot dashboard.
// =============================================================================

app.get(['/api/whatsapp/status', '/api/whatsapp/qr', '/api/status'], async (_req: Request, res: Response) => {
  const qrDataURL =
    ramStore.whatsapp.qr_code
      ? ramStore.whatsapp.qr_data_url || (await QRCode.toDataURL(ramStore.whatsapp.qr_code, { width: 220, margin: 1 }))
      : '';

  res.json({
    status: ramStore.whatsapp.status,
    needs_login: ramStore.whatsapp.needs_login,
    has_session: ramStore.whatsapp.has_session,
    qr: ramStore.whatsapp.qr_code,
    qr_data_url: qrDataURL,
    jid: ramStore.whatsapp.device_jid,
    device_jid: ramStore.whatsapp.device_jid,
    phone: ramStore.whatsapp.phone,
    connected: ramStore.whatsapp.status === 'Connected',
    whatsmeow_active: false,
    vendor: ramStore.vendor.business_name,
    vendor_id: ramStore.vendor.vendor_id,
    bot_public_url: botPublicURL,
    timestamp: new Date().toISOString(),
  });
});

app.post(['/api/whatsapp/pair', '/api/pair'], (req: Request, res: Response) => {
  const cleanPhone = normalizePhone(req.body?.phone || '');
  if (!cleanPhone || cleanPhone.length < 10) {
    res.status(400).json({ status: 'error', message: 'Valid phone number with country code is required.' });
    return;
  }
  res.status(503).json({
    status: 'error',
    message: 'Live 8-digit WhatsApp pairing requires the Go WhatsMeow engine running in deployment (Render / Railway / Koyeb).',
  });
});

app.post(['/api/whatsapp/logout', '/api/whatsapp/unlink'], (_req: Request, res: Response) => {
  ramStore.whatsapp = {
    has_session: false,
    needs_login: true,
    status: 'Needs_Login',
    device_jid: '',
    phone: '',
    push_name: '',
    qr_code: '',
    qr_data_url: '',
  };
  void pushToCentralAsync('/api/vendor-bot/whatsapp/session', {
    has_session: false,
    status: 'Disconnected',
    session_blob_base64: '',
  });
  pushLiveEvent('', 'WhatsApp session unlinked', 'session_unlinked');
  res.json({ status: 'success', message: 'WhatsApp unlinked and session cleared from Central Brain Node.' });
});

// =============================================================================
// 8. AI PLAYGROUND, LIVE CHATS & SSE STREAM
// =============================================================================

app.get('/api/ai/settings', (_req, res) => {
  res.json({
    status: 'success',
    business_name: ramStore.vendor.business_name,
    business_type: ramStore.vendor.business_type,
    response_language: ramStore.vendor.language,
    store_policies: ramStore.vendor.store_policies,
    ai_provider: ramStore.ai.provider,
    ai_model: ramStore.ai.model,
    ai_temperature: ramStore.ai.temperature,
    system_prompt: ramStore.ai.system_prompt,
    auto_reply: ramStore.ai.auto_reply,
    api_key_count: allAIKeys().length,
    allow_cod: ramStore.vendor.allow_cod,
  });
});

app.post('/api/ai/settings', (req: Request, res: Response) => {
  const b = req.body || {};
  if (typeof b.business_name === 'string' && b.business_name.trim()) ramStore.vendor.business_name = b.business_name.trim();
  if (typeof b.business_type === 'string') ramStore.vendor.business_type = b.business_type.trim();
  if (typeof b.notification_phone === 'string') ramStore.vendor.notification_phone = b.notification_phone.trim();
  if (typeof b.allow_cod === 'boolean') ramStore.vendor.allow_cod = b.allow_cod;
  if (typeof b.language === 'string') ramStore.vendor.language = b.language.trim();
  if (typeof b.store_policies === 'string') ramStore.vendor.store_policies = b.store_policies;
  if (typeof b.ai_provider === 'string' && b.ai_provider.trim()) ramStore.ai.provider = b.ai_provider.trim();
  if (typeof b.api_key === 'string' && b.api_key.trim()) ramStore.ai.api_key = b.api_key.trim();
  if (Array.isArray(b.api_keys)) ramStore.ai.api_keys = b.api_keys;
  if (Array.isArray(b.backup_api_keys)) ramStore.ai.backup_api_keys = b.backup_api_keys;
  if (typeof b.ai_model === 'string' && b.ai_model.trim()) ramStore.ai.model = b.ai_model.trim();
  if (typeof b.ai_temperature === 'number' && Number.isFinite(b.ai_temperature)) ramStore.ai.temperature = b.ai_temperature;
  if (typeof b.system_prompt === 'string') ramStore.ai.system_prompt = b.system_prompt;
  if (typeof b.auto_reply === 'boolean') ramStore.ai.auto_reply = b.auto_reply;
  if (typeof b.import_catalog === 'boolean') ramStore.ai.import_catalog = b.import_catalog;
  pushLiveEvent('', 'ai_settings', 'ram_synced');
  res.json({ status: 'success', api_key_count: allAIKeys().length, message: 'AI & Store settings updated in RAM.' });
});

app.post('/api/ai/test', async (req: Request, res: Response) => {
  const query = String(req.body?.query || '').trim();
  const phone = normalizePhone(req.body?.customer_phone || '');
  const mediaType = String(req.body?.media_type || 'text').toLowerCase();
  if (!query || !phone) {
    res.status(400).json({ status: 'error', message: 'query and customer_phone are required' });
    return;
  }

  const { customer: cust, changed: custChanged } = upsertCustomerWithChangeCheckRAM({ wa_number: phone, source: 'whatsapp' });
  if (cust && custChanged) {
    void pushToCentralAsync('/api/vendor-bot/events/customer', { event: 'customer.upserted', source: 'whatsapp', customer: cust });
  }

  const displayIncoming =
    mediaType === 'voice_note'
      ? `🎙️ [Voice Note]: ${query}`
      : mediaType === 'burst'
        ? `⚡ [Debounced Burst]: ${query}`
        : query;

  appendChatRAM(phone, 'customer', 'incoming', displayIncoming);
  pushLiveEvent(phone, displayIncoming, 'incoming');

  const result = await runOctopusAI(phone, query, req.body?.provider, req.body?.api_key, mediaType);
  let finalReply = result.ai_reply_text;
  let stockRejections: string[] = [];

  for (const act of result.actions) {
    if (act.hand === 'update_cart' && Array.isArray(act.payload?.cart_items)) {
      const incoming = act.payload.cart_items.map((ci: any) => ({
        product_id: String(ci.id || ci.product_id || ''),
        qty: Number(ci.qty || 1),
        selected_variant: ci.variant || ci.selected_variant || undefined,
        source: 'whatsapp',
      }));
      const updated = upsertCartValidatedRAM(phone, incoming, false, true);
      stockRejections = updated.rejected;
      void pushToCentralAsync('/api/vendor-bot/events/cart', {
        event: 'cart.updated',
        source: 'whatsapp',
        customer_phone: phone,
        jid: `${phone}@s.whatsapp.net`,
        items: updated.items,
        total_amount: updated.total,
      });
    } else if (act.hand === 'extract_profile' && act.payload?.profile) {
      const p = act.payload.profile;
      const { customer: savedCust, changed: profileChanged } = upsertCustomerWithChangeCheckRAM({
        wa_number: phone,
        full_name: p.name,
        delivery_address: p.address || p.delivery_address,
        contact_phone: p.contact || p.delivery_contact_10_digit,
        source: 'whatsapp',
      });
      if (savedCust && profileChanged) {
        void pushToCentralAsync('/api/vendor-bot/events/customer', { event: 'customer.upserted', source: 'whatsapp', customer: savedCust });
      }
    } else if (act.hand === 'submit_order') {
      const mode = String(act.payload?.payment_mode || 'ONLINE');
      const centralResp = await submitWhatsAppOrderToCentral(phone, mode);
      finalReply = centralResp.reply;
    } else if (act.hand === 'clear_cart') {
      const hadItems = (ramStore.activeCarts.get(phone) || []).length > 0;
      ramStore.activeCarts.delete(phone);
      if (hadItems) {
        void pushToCentralAsync('/api/vendor-bot/events/cart', {
          event: 'cart.updated',
          source: 'whatsapp',
          customer_phone: phone,
          items: [],
          total_amount: 0,
        });
      }
    }
  }

  if (stockRejections.length > 0) {
    finalReply += `\n\n⚠️ *Stock Note:* ${stockRejections.join('; ')}`;
  }

  appendChatRAM(phone, 'assistant', 'outgoing_ai', finalReply);
  pushLiveEvent(phone, finalReply, 'outgoing_ai');

  res.json({
    status: 'success',
    reply: finalReply,
    actions: result.actions,
    engine: result.engine,
    stock_rejections: stockRejections,
    cart: ramStore.activeCarts.get(phone) || [],
  });
});

app.get('/api/chats', (_req, res) => {
  const conversations = Array.from(ramStore.chats.entries())
    .filter(([, msgs]) => msgs.length > 0)
    .map(([phone, msgs]) => {
      const last = msgs[msgs.length - 1];
      const cust = ramStore.customers.get(phone);
      return {
        customer_phone: phone,
        customer_name: cust?.full_name || '',
        last_message: last.text,
        last_role: last.role,
        last_time: last.time,
        total_count: msgs.length,
      };
    })
    .reverse();
  res.json({ conversations });
});

app.get('/api/chats/messages', (req: Request, res: Response) => {
  const phone = normalizePhone(String(req.query.phone || ''));
  res.json({ messages: phone ? ramStore.chats.get(phone) || [] : [] });
});

app.post('/api/chats/send', (req: Request, res: Response) => {
  const phone = normalizePhone(req.body?.phone || '');
  const message = String(req.body?.message || '').trim();
  if (!phone || !message) {
    res.status(400).json({ status: 'error', message: 'phone and message are required' });
    return;
  }
  appendChatRAM(phone, 'assistant', 'outgoing_manual', message);
  pushLiveEvent(phone, message, 'outgoing_manual');
  res.json({ status: 'success', message: 'Reply recorded strictly in local RAM (zero Central chat sync).' });
});

app.get('/api/chat/stream', (req: Request, res: Response) => {
  res.setHeader('Content-Type', 'text/event-stream');
  res.setHeader('Cache-Control', 'no-cache');
  res.setHeader('Connection', 'keep-alive');
  res.flushHeaders?.();
  res.write(`data: ${JSON.stringify({ type: 'connected' })}\n\n`);
  sseClients.add(res);
  req.on('close', () => sseClients.delete(res));
});

// Serve Dashboard UI
app.get('/', (_req: Request, res: Response) => {
  res.sendFile(path.join(__dirname, 'public', 'index.html'));
});

app.use((req: Request, res: Response) => {
  if (req.path.startsWith('/api/')) {
    res.status(404).json({ status: 'error', message: `Endpoint not found: ${req.method} ${req.path}` });
    return;
  }
  res.sendFile(path.join(__dirname, 'public', 'index.html'));
});

// Boot Server + One-Time Startup Bootstrap (Zero Periodic Polling, Zero Recurring Heartbeat)
const server = app.listen(PORT, '0.0.0.0', () => {
  console.log(`🚀 Stateless Vendor WhatsApp Bot Node v2.3 running on http://0.0.0.0:${PORT}`);
  console.log(`🧠 Storage: 100% RAM-First | Chats: RAM-Only (Max 10) | Sync: Event-Driven (Zero Polling)`);
  void bootstrapFromCentral();
});

// Zero-Downtime Redeploy Graceful Shutdown
async function gracefulShutdown(signal: string) {
  console.log(`🛑 Received ${signal}. Flushing retry buffer before exit...`);
  server.close();
  await replayRetryBuffer();
  process.exit(0);
}
process.on('SIGTERM', () => void gracefulShutdown('SIGTERM'));
process.on('SIGINT', () => void gracefulShutdown('SIGINT'));
