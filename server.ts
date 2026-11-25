import express, { Request, Response, NextFunction } from 'express';
import cors from 'cors';
import crypto from 'crypto';
import path from 'path';
import { fileURLToPath } from 'url';
import QRCode from 'qrcode';
import sharp from 'sharp';
import { GoogleGenAI } from '@google/genai';

// CRITICAL for Render Free Tier 512MB RAM: disable libvips persistent decoded bitmap cache
sharp.cache(false);

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
  store_policies: string;
  auto_reply: boolean;
  import_catalog: boolean;
}

type BotMode = 'vendor_store' | 'admin_vendor_manager';

interface VendorManagementSettings {
  auto_renewal_reminders: boolean;
  renewal_reminder_days: number[];
  auto_onboarding_welcome: boolean;
  whatsapp_plan_renewal_qr: boolean;
  vendor_self_service_commands: boolean;
  mall_approval_alerts: boolean;
  support_escalation_phone: string;
  welcome_template: string;
  renewal_template: string;
}

interface OwnerPaymentGateway {
  upi_vpa: string;
  paytm_mid: string;
  payee_name: string;
}

interface SubscriptionPlan {
  id: string;
  name: string;
  monthly_price: number;
  yearly_price: number;
  validity_days: number;
}

interface ManagedVendor {
  username: string;
  store_name: string;
  owner_phone: string;
  plan_id: string;
  plan_name: string;
  plan_valid_until: string;
  days_remaining: number;
  is_plan_expired: boolean;
  upi_id: string;
  bot_connected: boolean;
  bot_public_url: string;
  store_url: string;
}

interface VendorManagementState {
  settings: VendorManagementSettings;
  owner_payment_gateway: OwnerPaymentGateway;
  subscription_plans: SubscriptionPlan[];
  managed_vendors: ManagedVendor[];
}

function normalizeBotMode(raw: unknown): BotMode {
  const s = String(raw || '').trim().toLowerCase();
  if (s === 'admin_vendor_manager' || s === 'admin-vendor-manager' || s === 'vendor_manager' || s === 'admin') {
    return 'admin_vendor_manager';
  }
  return 'vendor_store';
}

function defaultVendorManagementState(): VendorManagementState {
  return {
    settings: {
      auto_renewal_reminders: false,
      renewal_reminder_days: [7, 3, 1],
      auto_onboarding_welcome: false,
      whatsapp_plan_renewal_qr: true,
      vendor_self_service_commands: true,
      mall_approval_alerts: false,
      support_escalation_phone: '',
      welcome_template: '',
      renewal_template: '',
    },
    owner_payment_gateway: {
      upi_vpa: '',
      paytm_mid: '',
      payee_name: 'WaMart',
    },
    subscription_plans: [],
    managed_vendors: [],
  };
}

function phoneLookupKeys(raw: string): string[] {
  const digits = String(raw || '').replace(/\D/g, '');
  if (!digits) return [];
  const keys = new Set<string>();
  keys.add(digits);
  if (digits.length >= 10) {
    const last10 = digits.slice(-10);
    keys.add(last10);
    keys.add('91' + last10);
  }
  const norm = normalizePhone(raw);
  if (norm) keys.add(norm);
  return Array.from(keys);
}

const DEFAULT_PAYTM_STATUS_URL = 'https://securegw.paytm.in/order/status';
const DEFAULT_PAYMENT_CONFIRM_ENDPOINT = '/api/vendor-bot/events/payment-confirmed';
const DEFAULT_VERIFY_ENDPOINT = '/api/payments/verify';
const DEFAULT_VERIFY_POLL_INTERVAL_MS = 3000;
const DEFAULT_VERIFY_POLL_TIMEOUT_MS = 300000;
const DEFAULT_BANNER_QR_SIZE = 480;
const DEFAULT_BANNER_QR_OFFSET_X = 0;
const DEFAULT_BANNER_QR_OFFSET_Y = -130;
const DEFAULT_BANNER_QR_PADDING = 14;
const MAX_BLANK_BANNER_BYTES = 5 * 1024 * 1024;

interface BannerTemplate {
  enabled: boolean;
  banner_url: string;
  banner_width: number;
  banner_height: number;
  qr_size: number;
  qr_offset_x: number;
  qr_offset_y: number;
  qr_padding: number;
  updated_at: string;
}

function defaultBannerTemplate(): BannerTemplate {
  return {
    enabled: false,
    banner_url: '',
    banner_width: 0,
    banner_height: 0,
    qr_size: DEFAULT_BANNER_QR_SIZE,
    qr_offset_x: DEFAULT_BANNER_QR_OFFSET_X,
    qr_offset_y: DEFAULT_BANNER_QR_OFFSET_Y,
    qr_padding: DEFAULT_BANNER_QR_PADDING,
    updated_at: '',
  };
}

interface PaymentConfig {
  enabled: boolean;
  gateway: string;
  merchant_phone: string;
  upi_id: string;
  merchant_vpa: string;
  paytm_mid: string;
  payee_name: string;
  local_qr_generation: boolean;
  local_paytm_verify: boolean;
  paytm_status_url: string;
  payment_confirm_endpoint: string;
  qr_only_mode: boolean;
  send_payment_link: boolean;
  verify_endpoint: string;
  verify_poll_interval_ms: number;
  verify_poll_timeout_ms: number;
  banner_template: BannerTemplate;
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
  tags?: string[];
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
    tags: parseFlexStringArray(firstDefined(raw, 'tags', 'aliases', 'search_tags', 'keywords')),
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

  botMode: normalizeBotMode(process.env.BOT_MODE) as BotMode,
  vendorManagement: defaultVendorManagementState() as VendorManagementState,
  managedVendorsByPhone: new Map<string, ManagedVendor>(),

  ai: {
    provider: '',
    api_key: process.env.GROQ_API_KEY || process.env.GEMINI_API_KEY || process.env.OPENAI_API_KEY || '',
    api_keys: [],
    backup_api_keys: [],
    model: '',
    temperature: 0.7,
    system_prompt: '',
    store_policies: '',
    auto_reply: true,
    import_catalog: true,
  } as AIConfig,

  payment: {
    enabled: false,
    gateway: '',
    merchant_phone: '',
    upi_id: '',
    merchant_vpa: '',
    paytm_mid: '',
    payee_name: '',
    local_qr_generation: true,
    local_paytm_verify: true,
    paytm_status_url: DEFAULT_PAYTM_STATUS_URL,
    payment_confirm_endpoint: DEFAULT_PAYMENT_CONFIRM_ENDPOINT,
    qr_only_mode: true,
    send_payment_link: false,
    verify_endpoint: DEFAULT_VERIFY_ENDPOINT,
    verify_poll_interval_ms: DEFAULT_VERIFY_POLL_INTERVAL_MS,
    verify_poll_timeout_ms: DEFAULT_VERIFY_POLL_TIMEOUT_MS,
    banner_template: defaultBannerTemplate(),
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

const MAX_CHATS_PER_CUSTOMER = 10;

function appendChatRAM(phone: string, role: 'customer' | 'assistant', direction: string, text: string): ChatRecord | null {
  const cleanPhone = normalizePhone(phone);
  const cleanText = String(text || '').trim();
  if (!cleanPhone || !cleanText) return null;

  const list = ramStore.chats.get(cleanPhone) || [];
  if (list.length > 0) {
    const last = list[list.length - 1];
    if (last.role === role && last.text === cleanText && Date.now() - Date.parse(last.created_at) < 10000) {
      return last;
    }
  }

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
  list.push(rec);
  // Keep strictly the last 10 messages per customer in RAM (zero Central Node chat sync)
  if (list.length > MAX_CHATS_PER_CUSTOMER) list.splice(0, list.length - MAX_CHATS_PER_CUSTOMER);
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
    store_policies: firstDefined(a, 'store_policies', 'policies') !== undefined
      ? parseFlexString(firstDefined(a, 'store_policies', 'policies'))
      : curr.store_policies,
    auto_reply: parseFlexBool(firstDefined(a, 'auto_reply', 'ai_auto_reply', 'enabled'), curr.auto_reply),
    import_catalog: parseFlexBool(firstDefined(a, 'import_catalog', 'import_catalog_details'), curr.import_catalog),
  };
}

function applyVendorManagementSection(vmRaw: Record<string, any>): void {
  const prev = ramStore.vendorManagement;
  const next: VendorManagementState = {
    settings: { ...prev.settings },
    owner_payment_gateway: { ...prev.owner_payment_gateway },
    subscription_plans: [...prev.subscription_plans],
    managed_vendors: [...prev.managed_vendors],
  };

  const sRaw = firstDefined(vmRaw, 'settings');
  if (isPlainObject(sRaw)) {
    const daysRaw = firstDefined(sRaw, 'renewal_reminder_days');
    let days = next.settings.renewal_reminder_days;
    if (Array.isArray(daysRaw)) {
      days = daysRaw
        .map((n) => Math.round(parseFlexNumber(n, 0)))
        .filter((n) => n >= 0);
    } else if (typeof daysRaw === 'string' && daysRaw.trim()) {
      days = daysRaw
        .split(',')
        .map((part) => Math.round(parseFlexNumber(part.trim(), -1)))
        .filter((n) => n >= 0);
    }
    next.settings = {
      auto_renewal_reminders: parseFlexBool(firstDefined(sRaw, 'auto_renewal_reminders'), next.settings.auto_renewal_reminders),
      renewal_reminder_days: days,
      auto_onboarding_welcome: parseFlexBool(firstDefined(sRaw, 'auto_onboarding_welcome'), next.settings.auto_onboarding_welcome),
      whatsapp_plan_renewal_qr: parseFlexBool(firstDefined(sRaw, 'whatsapp_plan_renewal_qr'), next.settings.whatsapp_plan_renewal_qr),
      vendor_self_service_commands: parseFlexBool(firstDefined(sRaw, 'vendor_self_service_commands'), next.settings.vendor_self_service_commands),
      mall_approval_alerts: parseFlexBool(firstDefined(sRaw, 'mall_approval_alerts'), next.settings.mall_approval_alerts),
      support_escalation_phone: firstDefined(sRaw, 'support_escalation_phone') !== undefined
        ? parseFlexString(firstDefined(sRaw, 'support_escalation_phone'))
        : next.settings.support_escalation_phone,
      welcome_template: firstDefined(sRaw, 'welcome_template') !== undefined
        ? parseFlexString(firstDefined(sRaw, 'welcome_template'))
        : next.settings.welcome_template,
      renewal_template: firstDefined(sRaw, 'renewal_template') !== undefined
        ? parseFlexString(firstDefined(sRaw, 'renewal_template'))
        : next.settings.renewal_template,
    };
  }

  const gwRaw = firstDefined(vmRaw, 'owner_payment_gateway', 'payment_gateway');
  if (isPlainObject(gwRaw)) {
    next.owner_payment_gateway = {
      upi_vpa: firstDefined(gwRaw, 'upi_vpa', 'upi_id', 'vpa', 'merchant_vpa') !== undefined
        ? parseFlexString(firstDefined(gwRaw, 'upi_vpa', 'upi_id', 'vpa', 'merchant_vpa'))
        : next.owner_payment_gateway.upi_vpa,
      paytm_mid: firstDefined(gwRaw, 'paytm_mid', 'mid', 'merchant_id') !== undefined
        ? parseFlexString(firstDefined(gwRaw, 'paytm_mid', 'mid', 'merchant_id'))
        : next.owner_payment_gateway.paytm_mid,
      payee_name: firstDefined(gwRaw, 'payee_name', 'merchant_name') !== undefined
        ? parseFlexString(firstDefined(gwRaw, 'payee_name', 'merchant_name'))
        : next.owner_payment_gateway.payee_name || 'WaMart',
    };
  }

  const plansRaw = firstDefined(vmRaw, 'subscription_plans', 'plans');
  if (Array.isArray(plansRaw)) {
    next.subscription_plans = plansRaw
      .filter((p) => isPlainObject(p))
      .map((p: Record<string, any>) => ({
        id: parseFlexString(firstDefined(p, 'id', 'plan_id')),
        name: parseFlexString(firstDefined(p, 'name', 'plan_name')),
        monthly_price: parseFlexNumber(firstDefined(p, 'monthly_price', 'price'), 0),
        yearly_price: parseFlexNumber(firstDefined(p, 'yearly_price'), 0),
        validity_days: Math.round(parseFlexNumber(firstDefined(p, 'validity_days', 'duration_days'), 30)),
      }))
      .filter((p) => Boolean(p.id || p.name));
  }

  const vendorsRaw = firstDefined(vmRaw, 'managed_vendors', 'vendors');
  if (Array.isArray(vendorsRaw)) {
    next.managed_vendors = vendorsRaw
      .filter((v) => isPlainObject(v))
      .map((v: Record<string, any>) => ({
        username: parseFlexString(firstDefined(v, 'username')),
        store_name: parseFlexString(firstDefined(v, 'store_name', 'business_name', 'name')),
        owner_phone: parseFlexString(firstDefined(v, 'owner_phone', 'phone', 'notification_phone')),
        plan_id: parseFlexString(firstDefined(v, 'plan_id')),
        plan_name: parseFlexString(firstDefined(v, 'plan_name')),
        plan_valid_until: parseFlexString(firstDefined(v, 'plan_valid_until', 'subscription_valid_until')),
        days_remaining: Math.round(parseFlexNumber(firstDefined(v, 'days_remaining'), 0)),
        is_plan_expired: parseFlexBool(firstDefined(v, 'is_plan_expired'), false),
        upi_id: parseFlexString(firstDefined(v, 'upi_id', 'merchant_vpa')),
        bot_connected: parseFlexBool(firstDefined(v, 'bot_connected'), false),
        bot_public_url: parseFlexString(firstDefined(v, 'bot_public_url')),
        store_url: parseFlexString(firstDefined(v, 'store_url')),
      }));
  }

  ramStore.vendorManagement = next;
  ramStore.managedVendorsByPhone.clear();
  for (const mv of next.managed_vendors) {
    for (const key of phoneLookupKeys(mv.owner_phone)) {
      ramStore.managedVendorsByPhone.set(key, mv);
    }
  }
}

function lookupManagedVendorByPhone(phone: string): ManagedVendor | undefined {
  for (const key of phoneLookupKeys(phone)) {
    const found = ramStore.managedVendorsByPhone.get(key);
    if (found) return found;
  }
  return undefined;
}

// Ultra-Low-RAM Blank Banner Raw Compressed Buffer Cache (~150-300 KB max, NEVER decoded RGBA bitmap)
const blankBannerCache: {
  rawBuffer: Buffer | null;
  url: string;
  updatedAt: string;
  inFlightKey: string;
} = {
  rawBuffer: null,
  url: '',
  updatedAt: '',
  inFlightKey: '',
};

function clearBlankBannerCache(): void {
  if (blankBannerCache.rawBuffer) {
    console.log('🧹 [BannerCache] Cleared blank banner bytes from RAM');
  }
  blankBannerCache.rawBuffer = null;
  blankBannerCache.url = '';
  blankBannerCache.updatedAt = '';
  blankBannerCache.inFlightKey = '';
}

function getBlankBannerCacheStats(): { cached: boolean; size_bytes: number; url: string; updated_at: string } {
  return {
    cached: Boolean(blankBannerCache.rawBuffer && blankBannerCache.rawBuffer.length > 0),
    size_bytes: blankBannerCache.rawBuffer ? blankBannerCache.rawBuffer.length : 0,
    url: blankBannerCache.url,
    updated_at: blankBannerCache.updatedAt,
  };
}

async function downloadCompressedBannerBuffer(bannerUrl: string): Promise<Buffer> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 15000);
  try {
    const res = await fetch(bannerUrl, {
      method: 'GET',
      headers: { 'User-Agent': 'VendorWhatsAppBotNode/2.5' },
      signal: ctrl.signal,
    });
    if (!res.ok) {
      throw new Error(`HTTP ${res.status} downloading blank banner`);
    }
    const ab = await res.arrayBuffer();
    const buf = Buffer.from(ab);
    if (buf.length === 0) {
      throw new Error('empty blank banner response');
    }
    if (buf.length > MAX_BLANK_BANNER_BYTES) {
      throw new Error('blank banner exceeds 5MB RAM safety cap');
    }
    return buf;
  } finally {
    clearTimeout(timer);
  }
}

function syncBlankBannerCacheAsync(tpl: BannerTemplate): void {
  const cleanUrl = (tpl.banner_url || '').trim();
  const cleanUpd = (tpl.updated_at || '').trim();
  if (!tpl.enabled || !cleanUrl) {
    clearBlankBannerCache();
    return;
  }
  const key = `${cleanUrl}|${cleanUpd}`;
  if (blankBannerCache.rawBuffer && blankBannerCache.url === cleanUrl && blankBannerCache.updatedAt === cleanUpd) {
    return;
  }
  if (blankBannerCache.inFlightKey === key) return;
  blankBannerCache.inFlightKey = key;

  void downloadCompressedBannerBuffer(cleanUrl)
    .then((buf) => {
      blankBannerCache.rawBuffer = buf;
      blankBannerCache.url = cleanUrl;
      blankBannerCache.updatedAt = cleanUpd;
      if (blankBannerCache.inFlightKey === key) blankBannerCache.inFlightKey = '';
      console.log(`🖼️ [BannerCache] Cached blank banner raw bytes in RAM (${(buf.length / 1024).toFixed(1)} KB)`);
    })
    .catch((err: any) => {
      if (blankBannerCache.inFlightKey === key) blankBannerCache.inFlightKey = '';
      console.warn(`⚠️ [BannerCache] Failed downloading blank banner (${cleanUrl}): ${err?.message || String(err)}`);
    });
}

async function getOrFetchBlankBannerBuffer(tpl: BannerTemplate): Promise<Buffer | null> {
  const cleanUrl = (tpl.banner_url || '').trim();
  const cleanUpd = (tpl.updated_at || '').trim();
  if (!tpl.enabled || !cleanUrl) {
    clearBlankBannerCache();
    return null;
  }
  if (blankBannerCache.rawBuffer && blankBannerCache.url === cleanUrl && blankBannerCache.updatedAt === cleanUpd) {
    return blankBannerCache.rawBuffer;
  }
  try {
    const buf = await downloadCompressedBannerBuffer(cleanUrl);
    blankBannerCache.rawBuffer = buf;
    blankBannerCache.url = cleanUrl;
    blankBannerCache.updatedAt = cleanUpd;
    blankBannerCache.inFlightKey = '';
    return buf;
  } catch (err: any) {
    console.warn(`⚠️ [BannerCache] On-demand banner fetch failed: ${err?.message || String(err)}`);
    if (blankBannerCache.rawBuffer && blankBannerCache.url === cleanUrl) {
      return blankBannerCache.rawBuffer;
    }
    return null;
  }
}

function parseOptionalInt(val: any): number | undefined {
  if (val === undefined || val === null || val === '') return undefined;
  const n = Number(val);
  return Number.isFinite(n) ? Math.round(n) : undefined;
}

function applyPaymentSection(p: Record<string, any>): void {
  const curr = ramStore.payment;
  const gateway = firstDefined(p, 'gateway', 'provider', 'payment_gateway') !== undefined
    ? parseFlexString(firstDefined(p, 'gateway', 'provider', 'payment_gateway'))
    : curr.gateway;
  const upiRaw = firstDefined(p, 'upi_id', 'merchant_vpa', 'paytm_vpa', 'vpa');
  const merchantVpaRaw = firstDefined(p, 'merchant_vpa', 'upi_id', 'paytm_vpa', 'vpa');
  const upiId = upiRaw !== undefined ? parseFlexString(upiRaw) : (curr.upi_id || curr.merchant_vpa);
  const merchantVpa = merchantVpaRaw !== undefined ? parseFlexString(merchantVpaRaw) : (upiId || curr.merchant_vpa);
  const paytmMid = firstDefined(p, 'paytm_mid', 'mid', 'merchant_id') !== undefined
    ? parseFlexString(firstDefined(p, 'paytm_mid', 'mid', 'merchant_id'))
    : curr.paytm_mid;
  const payeeRaw = firstDefined(p, 'payee_name', 'merchant_name', 'upi_name');
  const payeeName = (payeeRaw !== undefined ? parseFlexString(payeeRaw) : curr.payee_name) || ramStore.vendor.business_name;
  const merchantPhone = firstDefined(p, 'merchant_phone', 'phone') !== undefined
    ? parseFlexString(firstDefined(p, 'merchant_phone', 'phone'))
    : curr.merchant_phone;

  const localQrOpt = parseFlexBoolOpt(firstDefined(p, 'local_qr_generation'));
  const localQrGeneration = localQrOpt !== undefined ? localQrOpt : (curr.local_qr_generation ?? true);
  const localPaytmOpt = parseFlexBoolOpt(firstDefined(p, 'local_paytm_verify'));
  const localPaytmVerify = localPaytmOpt !== undefined ? localPaytmOpt : (curr.local_paytm_verify ?? true);

  const paytmStatusRaw = firstDefined(p, 'paytm_status_url');
  const paytmStatusUrl = (paytmStatusRaw !== undefined ? parseFlexString(paytmStatusRaw) : curr.paytm_status_url) || DEFAULT_PAYTM_STATUS_URL;

  const confirmEpRaw = firstDefined(p, 'payment_confirm_endpoint');
  const paymentConfirmEndpoint = (confirmEpRaw !== undefined ? parseFlexString(confirmEpRaw) : curr.payment_confirm_endpoint) || DEFAULT_PAYMENT_CONFIRM_ENDPOINT;

  const qrOnlyOpt = parseFlexBoolOpt(firstDefined(p, 'qr_only_mode'));
  const qrOnlyMode = qrOnlyOpt !== undefined ? qrOnlyOpt : (curr.qr_only_mode ?? true);
  const sendLinkOpt = parseFlexBoolOpt(firstDefined(p, 'send_payment_link'));
  const sendPaymentLink = sendLinkOpt !== undefined ? sendLinkOpt : (curr.send_payment_link ?? false);
  const verifyEpRaw = firstDefined(p, 'verify_endpoint');
  const verifyEndpoint = (verifyEpRaw !== undefined ? parseFlexString(verifyEpRaw) : curr.verify_endpoint) || DEFAULT_VERIFY_ENDPOINT;
  const pollIntervalRaw = firstDefined(p, 'verify_poll_interval_ms');
  const parsedInterval = pollIntervalRaw !== undefined ? Math.round(parseFlexNumber(pollIntervalRaw, curr.verify_poll_interval_ms || DEFAULT_VERIFY_POLL_INTERVAL_MS)) : (curr.verify_poll_interval_ms || DEFAULT_VERIFY_POLL_INTERVAL_MS);
  const verifyPollIntervalMs = parsedInterval > 0 ? parsedInterval : DEFAULT_VERIFY_POLL_INTERVAL_MS;
  const pollTimeoutRaw = firstDefined(p, 'verify_poll_timeout_ms');
  const parsedTimeout = pollTimeoutRaw !== undefined ? Math.round(parseFlexNumber(pollTimeoutRaw, curr.verify_poll_timeout_ms || DEFAULT_VERIFY_POLL_TIMEOUT_MS)) : (curr.verify_poll_timeout_ms || DEFAULT_VERIFY_POLL_TIMEOUT_MS);
  const verifyPollTimeoutMs = parsedTimeout > 0 ? parsedTimeout : DEFAULT_VERIFY_POLL_TIMEOUT_MS;

  // Parse payment.banner_template + universal_qr_banner_url / qr_banner_url fallbacks (preserving explicit 0 values for offset/padding)
  const currTpl = curr.banner_template || defaultBannerTemplate();
  let bannerTemplate: BannerTemplate = { ...currTpl };
  const rawTpl = firstDefined(p, 'banner_template');
  const fallbackBannerUrl = parseFlexString(firstDefined(p, 'universal_qr_banner_url', 'qr_banner_url', 'banner_url')).trim();
  if (rawTpl === null && !fallbackBannerUrl) {
    bannerTemplate = defaultBannerTemplate();
  } else if (isPlainObject(rawTpl)) {
    const tEnabled = parseFlexBoolOpt(firstDefined(rawTpl, 'enabled'));
    const tUrlRaw = firstDefined(rawTpl, 'banner_url', 'universal_qr_banner_url', 'qr_banner_url', 'url');
    const nextBannerUrl = tUrlRaw !== undefined ? parseFlexString(tUrlRaw) : (fallbackBannerUrl || currTpl.banner_url);
    const tW = parseOptionalInt(firstDefined(rawTpl, 'banner_width', 'width'));
    const tH = parseOptionalInt(firstDefined(rawTpl, 'banner_height', 'height'));
    const tSize = parseOptionalInt(firstDefined(rawTpl, 'qr_size'));
    const tOffX = parseOptionalInt(firstDefined(rawTpl, 'qr_offset_x'));
    const tOffY = parseOptionalInt(firstDefined(rawTpl, 'qr_offset_y'));
    const tPad = parseOptionalInt(firstDefined(rawTpl, 'qr_padding'));
    const tUpdRaw = firstDefined(rawTpl, 'updated_at');

    bannerTemplate = {
      enabled: tEnabled !== undefined ? tEnabled : (nextBannerUrl ? true : currTpl.enabled),
      banner_url: nextBannerUrl,
      banner_width: tW !== undefined ? tW : currTpl.banner_width,
      banner_height: tH !== undefined ? tH : currTpl.banner_height,
      qr_size: tSize !== undefined && tSize > 0 ? tSize : (currTpl.qr_size || DEFAULT_BANNER_QR_SIZE),
      qr_offset_x: tOffX !== undefined ? tOffX : currTpl.qr_offset_x,
      qr_offset_y: tOffY !== undefined ? tOffY : currTpl.qr_offset_y,
      qr_padding: tPad !== undefined && tPad >= 0 ? tPad : currTpl.qr_padding,
      updated_at: tUpdRaw !== undefined ? parseFlexString(tUpdRaw) : currTpl.updated_at,
    };
  } else if (fallbackBannerUrl) {
    bannerTemplate = {
      ...currTpl,
      enabled: true,
      banner_url: fallbackBannerUrl,
    };
  }

  const enabledOpt = parseFlexBoolOpt(firstDefined(p, 'enabled', 'online_payment_enabled', 'is_enabled', 'active'));
  const enabled = enabledOpt !== undefined ? enabledOpt : Boolean(gateway || merchantVpa || upiId || paytmMid || curr.enabled);

  ramStore.payment = {
    enabled,
    gateway,
    merchant_phone: merchantPhone,
    upi_id: upiId || merchantVpa,
    merchant_vpa: merchantVpa || upiId,
    paytm_mid: paytmMid,
    payee_name: payeeName,
    local_qr_generation: localQrGeneration,
    local_paytm_verify: localPaytmVerify,
    paytm_status_url: paytmStatusUrl,
    payment_confirm_endpoint: paymentConfirmEndpoint,
    qr_only_mode: qrOnlyMode,
    send_payment_link: sendPaymentLink,
    verify_endpoint: verifyEndpoint,
    verify_poll_interval_ms: verifyPollIntervalMs,
    verify_poll_timeout_ms: verifyPollTimeoutMs,
    banner_template: bannerTemplate,
  };

  // Sync compressed blank banner bytes asynchronously (or immediately free RAM if disabled)
  syncBlankBannerCacheAsync(bannerTemplate);
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

  // 0. Bot Mode (vendor_store vs admin_vendor_manager)
  const vRawForMode = firstDefined(data, 'vendor', 'business', 'store');
  const rawMode = firstDefined(data, 'bot_mode') ?? (isPlainObject(vRawForMode) ? firstDefined(vRawForMode, 'bot_mode') : undefined);
  if (rawMode !== undefined && String(rawMode).trim()) {
    ramStore.botMode = normalizeBotMode(rawMode);
  }

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

  // 3. Payment Config (also supports top-level universal_qr_banner_url / qr_banner_url fallbacks)
  const payRaw = firstDefined(data, 'payment', 'payment_config', 'payment_settings');
  const topBannerFallback = parseFlexString(firstDefined(data, 'universal_qr_banner_url', 'qr_banner_url')).trim();
  if (payRaw !== undefined || topBannerFallback) {
    try {
      if (payRaw !== undefined && !isPlainObject(payRaw)) throw new Error('payment section is not a JSON object');
      const mergedPay = isPlainObject(payRaw) ? { ...payRaw } : {};
      if (topBannerFallback && !mergedPay.universal_qr_banner_url) {
        mergedPay.universal_qr_banner_url = topBannerFallback;
      }
      applyPaymentSection(mergedPay);
    } catch (err: any) {
      warnings.push(`payment: ${err?.message || String(err)}`);
    }
  }

  // 3b. Vendor Management State (Admin Vendor Manager mode)
  const vmRaw = firstDefined(data, 'vendor_management');
  if (vmRaw !== undefined) {
    try {
      if (!isPlainObject(vmRaw)) throw new Error('vendor_management section is not a JSON object');
      applyVendorManagementSection(vmRaw);
    } catch (err: any) {
      warnings.push(`vendor_management: ${err?.message || String(err)}`);
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
      const blob = parseFlexString(
        firstDefined(wsRaw, 'session_blob_base64', 'session_blob', 'session_data', 'whatsapp_session_blob', 'blob', 'data_base64')
      ).trim();
      const hasSession = parseFlexBool(firstDefined(wsRaw, 'has_session', 'connected', 'is_linked'), Boolean(blob)) || Boolean(blob);
      const statusRaw = parseFlexString(firstDefined(wsRaw, 'status', 'state')) as WhatsAppSessionState['status'];
      ramStore.whatsapp = {
        has_session: hasSession,
        needs_login: !hasSession,
        status: hasSession ? statusRaw || 'Connected' : 'Needs_Login',
        device_jid: parseFlexString(firstDefined(wsRaw, 'device_jid', 'jid', 'wa_jid')),
        phone: normalizePhone(parseFlexString(firstDefined(wsRaw, 'phone', 'wa_number', 'number'))),
        push_name: parseFlexString(firstDefined(wsRaw, 'push_name', 'name')),
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
    managed_vendors: ramStore.vendorManagement.managed_vendors.length,
    subscription_plans: ramStore.vendorManagement.subscription_plans.length,
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
    signal: AbortSignal.timeout(35000),
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
    const bootstrapPayload = {
      bot_public_url: botPublicURL,
      bot_mode: ramStore.botMode,
      node_version: '2.5-dual-mode-vendor-manager',
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
        'branded_banner_qr_only',
        'payment_verify_polling',
        'dual_mode_admin_vendor_manager',
      ],
      timestamp: new Date().toISOString(),
    };
    let data: any;
    try {
      data = await callCentralNode('POST', '/api/node/bootstrap', bootstrapPayload);
    } catch {
      try {
        data = await callCentralNode('POST', '/api/vendor-bot/bootstrap', bootstrapPayload);
      } catch {
        data = await callCentralNode('GET', '/api/vendor-bot/bootstrap');
      }
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
// 3. CENTRAL-BRAIN BRANDED BANNER QR-ONLY PAYMENT & 3-SECOND VERIFY POLLING
//    Bot NEVER sends upi:// links or checkout URLs — ONLY Branded Banner QR.
// =============================================================================

interface ActivePollerEntry {
  timer: NodeJS.Timeout;
  timeout: NodeJS.Timeout;
}

const activePaymentPollers = new Map<string, ActivePollerEntry>();
const notifiedPaidOrders = new Map<string, number>();

function markOrderPaidNotifiedOnce(orderId: string): boolean {
  const key = String(orderId || '').trim();
  if (!key) return false;
  if (notifiedPaidOrders.has(key)) return false;
  const now = Date.now();
  notifiedPaidOrders.set(key, now);
  if (notifiedPaidOrders.size > 500) {
    const cutoff = now - 24 * 60 * 60 * 1000;
    for (const [k, ts] of notifiedPaidOrders.entries()) {
      if (ts < cutoff) notifiedPaidOrders.delete(k);
    }
  }
  return true;
}

function stopPaymentVerificationPoller(orderId: string): void {
  const key = String(orderId || '').trim();
  if (!key) return;
  const entry = activePaymentPollers.get(key);
  if (entry) {
    clearInterval(entry.timer);
    clearTimeout(entry.timeout);
    activePaymentPollers.delete(key);
    console.log(`🛑 [PaymentPoller] Stopped verification loop for Order #${key}`);
  }
}

// Build UPI string strictly for QR encoding ONLY — NEVER send as text or clickable link!
function buildUPIQRString(params: {
  upiQrData?: string;
  upiId: string;
  payeeName: string;
  orderId: string;
  amount: number;
}): string {
  const raw = (params.upiQrData || '').trim();
  if (raw) return raw;
  const vpa = (params.upiId || '').trim();
  if (!vpa) return '';
  const name = (params.payeeName || '').trim() || ramStore.vendor.business_name || 'Store';
  return `upi://pay?pa=${vpa}&pn=${encodeURIComponent(name)}&tr=${encodeURIComponent(params.orderId.trim())}&am=${params.amount.toFixed(2)}&cu=INR`;
}

// Local In-Memory Dynamic Branded Banner QR Compositor (qrcode + sharp with sharp.cache(false))
// Zero external QR Worker or Cloudinary dependency.
async function generateLocalBannerQRBuffer(upiStr: string, tpl: BannerTemplate): Promise<{ buffer: Buffer; mimeType: string }> {
  const cleanUri = (upiStr || '').trim();
  if (!cleanUri) {
    throw new Error('empty UPI QR data');
  }

  const bannerBytes = await getOrFetchBlankBannerBuffer(tpl);
  if (bannerBytes && bannerBytes.length > 0) {
    try {
      const meta = await sharp(bannerBytes).metadata();
      const bannerW = meta.width || tpl.banner_width || 1080;
      const bannerH = meta.height || tpl.banner_height || 1350;

      let qrSize = tpl.qr_size > 0 ? tpl.qr_size : DEFAULT_BANNER_QR_SIZE;
      const minDim = Math.min(bannerW, bannerH);
      if (minDim > 80 && qrSize > minDim - 20) {
        qrSize = minDim - 20;
      }
      const offsetX = Number.isFinite(tpl.qr_offset_x) ? tpl.qr_offset_x : DEFAULT_BANNER_QR_OFFSET_X;
      const offsetY = Number.isFinite(tpl.qr_offset_y) ? tpl.qr_offset_y : DEFAULT_BANNER_QR_OFFSET_Y;
      const pad = tpl.qr_padding >= 0 ? tpl.qr_padding : DEFAULT_BANNER_QR_PADDING;

      const qrX = Math.round((bannerW - qrSize) / 2) + offsetX;
      const qrY = Math.round((bannerH - qrSize) / 2) + offsetY;

      const qrPngBuf = await QRCode.toBuffer(cleanUri, {
        type: 'png',
        width: qrSize,
        margin: 0,
        errorCorrectionLevel: 'M',
        color: { dark: '#000000', light: '#FFFFFF' },
      });

      const quietW = qrSize + pad * 2;
      const quietH = qrSize + pad * 2;
      const quietBoxBuf = await sharp({
        create: {
          width: quietW,
          height: quietH,
          channels: 4,
          background: { r: 255, g: 255, b: 255, alpha: 1 },
        },
      })
        .png()
        .toBuffer();

      const composited = await sharp(bannerBytes)
        .composite([
          { input: quietBoxBuf, left: Math.max(0, qrX - pad), top: Math.max(0, qrY - pad) },
          { input: qrPngBuf, left: Math.max(0, qrX), top: Math.max(0, qrY) },
        ])
        .jpeg({ quality: 85 })
        .toBuffer();

      return { buffer: composited, mimeType: 'image/jpeg' };
    } catch (err: any) {
      console.warn(`⚠️ [LocalBannerQR] Banner composite fallback: ${err?.message || String(err)} — using standalone 500x500 PNG QR`);
    }
  }

  // Fallback: standalone crisp 500x500 PNG UPI QR
  const standalonePng = await QRCode.toBuffer(cleanUri, {
    type: 'png',
    width: 500,
    margin: 2,
    errorCorrectionLevel: 'M',
    color: { dark: '#000000', light: '#FFFFFF' },
  });
  return { buffer: standalonePng, mimeType: 'image/png' };
}

// Direct Paytm Status API check (5-second HTTP timeout):
// GET https://securegw.paytm.in/order/status?JsonData=<urlEncoded({"MID":"<mid>","ORDERID":"<order_id>"})>
async function checkPaytmStatusDirect(statusUrl: string, paytmMid: string, orderId: string): Promise<{
  STATUS?: string;
  TXNID?: string;
  TXNAMOUNT?: string;
  ORDERID?: string;
  RESPMSG?: string;
}> {
  const endpoint = (statusUrl || '').trim() || DEFAULT_PAYTM_STATUS_URL;
  const jsonData = JSON.stringify({ MID: paytmMid.trim(), ORDERID: orderId.trim() });
  const sep = endpoint.includes('?') ? '&' : '?';
  const reqUrl = `${endpoint}${sep}JsonData=${encodeURIComponent(jsonData)}`;

  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 5000);
  try {
    const res = await fetch(reqUrl, {
      method: 'GET',
      headers: { Accept: 'application/json', 'User-Agent': 'VendorWhatsAppBotNode/2.5' },
      signal: ctrl.signal,
    });
    const text = await res.text();
    if (!res.ok) {
      throw new Error(`Paytm HTTP ${res.status}: ${text.slice(0, 160)}`);
    }
    return text ? JSON.parse(text) : {};
  } finally {
    clearTimeout(timer);
  }
}

// 1-Shot Central Brain payment-confirmed sync with 3 retries & exponential backoff (1s, 2s, 4s)
async function pushPaymentConfirmedWithRetry(
  endpoint: string,
  evt: { order_id: string; txn_id: string; amount: number; paytm_status: string; customer_phone: string }
): Promise<void> {
  if (!centralNodeURL || !vendorApiKey) return;
  const pathStr = (endpoint || '').trim() || DEFAULT_PAYMENT_CONFIRM_ENDPOINT;
  const normalizedPath = pathStr.startsWith('http://') || pathStr.startsWith('https://') || pathStr.startsWith('/') ? pathStr : `/${pathStr}`;

  let delayMs = 1000;
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      if (normalizedPath.startsWith('http://') || normalizedPath.startsWith('https://')) {
        const res = await fetch(normalizedPath, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'X-Vendor-API-Key': vendorApiKey,
            Authorization: `Bearer ${vendorApiKey}`,
          },
          body: JSON.stringify(evt),
        });
        if (!res.ok) {
          throw new Error(`HTTP ${res.status}`);
        }
      } else {
        await callCentralNode('POST', normalizedPath, evt);
      }
      ramStore.sync.outbound_pushed += 1;
      ramStore.sync.last_sync_at = new Date().toISOString();
      console.log(`✅ [CentralPaymentSync] Confirmed Order #${evt.order_id} (txn=${evt.txn_id}) synced on attempt ${attempt}/3`);
      return;
    } catch (err: any) {
      console.warn(`⚠️ [CentralPaymentSync Attempt ${attempt}/3] Order #${evt.order_id} failed: ${err?.message || String(err)}`);
      if (attempt < 3) {
        await new Promise((r) => setTimeout(r, delayMs));
        delayMs *= 2;
      }
    }
  }
  if (!normalizedPath.startsWith('http://') && !normalizedPath.startsWith('https://')) {
    void pushToCentralAsync(normalizedPath, evt);
  }
}

function sendPaidOrderConfirmationPreview(params: {
  orderId: string;
  customerPhone: string;
  customerName: string;
  totalAmount: number;
  txnId?: string;
  deliveryAddress: string;
}): void {
  const lines = [
    `✅ *Payment & Order Confirmed!*`,
    ``,
    `• *Order ID:* #${params.orderId}`,
    `• *Amount Paid:* ₹${params.totalAmount.toFixed(2)}`,
  ];
  if (params.txnId && params.txnId.trim()) {
    lines.push(`• *Transaction ID:* ${params.txnId.trim()}`);
  }
  lines.push(`• *Status:* CONFIRMED & PAID`);
  if (params.deliveryAddress && params.deliveryAddress.trim()) {
    lines.push(`• *Delivery Address:* ${params.deliveryAddress.trim()}`);
  }
  lines.push(``, `🙏 Thank you for ordering with *${ramStore.vendor.business_name || 'our store'}*!`);
  const msg = lines.join('\n');

  appendChatRAM(params.customerPhone, 'assistant', 'outgoing_ai', msg);
  pushLiveEvent(params.customerPhone, msg, 'outgoing_ai');

  const notifyPhone = normalizePhone(ramStore.vendor.notification_phone || '');
  if (notifyPhone && notifyPhone !== normalizePhone(params.customerPhone)) {
    const custLabel = params.customerName || params.customerPhone;
    let vendorAlert = `🔔 *New Paid Order #${params.orderId}*\n• Customer: ${custLabel} (+${params.customerPhone})\n• Amount Paid: ₹${params.totalAmount.toFixed(2)}\n• Status: CONFIRMED & PAID`;
    if (params.txnId && params.txnId.trim()) {
      vendorAlert += `\n• Txn ID: ${params.txnId.trim()}`;
    }
    if (params.deliveryAddress && params.deliveryAddress.trim()) {
      vendorAlert += `\n• Address: ${params.deliveryAddress.trim()}`;
    }
    pushLiveEvent(notifyPhone, vendorAlert, 'outgoing_ai');
  }
}

function startPaymentVerificationPoller(params: {
  orderId: string;
  customerPhone: string;
  customerName: string;
  deliveryAddress: string;
  totalAmount: number;
  paytmMid: string;
}): void {
  const orderId = String(params.orderId || '').trim();
  if (!orderId) return;
  if (activePaymentPollers.has(orderId)) return;

  const payCfg = ramStore.payment;
  const intervalMs = payCfg.verify_poll_interval_ms > 0 ? payCfg.verify_poll_interval_ms : DEFAULT_VERIFY_POLL_INTERVAL_MS;
  const timeoutMs = payCfg.verify_poll_timeout_ms > 0 ? payCfg.verify_poll_timeout_ms : DEFAULT_VERIFY_POLL_TIMEOUT_MS;
  const paytmMid = (params.paytmMid || payCfg.paytm_mid || '').trim();
  const paytmStatusUrl = (payCfg.paytm_status_url || DEFAULT_PAYTM_STATUS_URL).trim() || DEFAULT_PAYTM_STATUS_URL;
  const confirmEndpoint = (payCfg.payment_confirm_endpoint || DEFAULT_PAYMENT_CONFIRM_ENDPOINT).trim() || DEFAULT_PAYMENT_CONFIRM_ENDPOINT;
  const verifyEndpoint = (payCfg.verify_endpoint || DEFAULT_VERIFY_ENDPOINT).trim() || DEFAULT_VERIFY_ENDPOINT;
  const useDirectPaytm = payCfg.local_paytm_verify !== false && Boolean(paytmMid);
  if (!useDirectPaytm && (!centralNodeURL || !vendorApiKey)) return;

  const cleanPhone = normalizePhone(params.customerPhone);

  let inFlight = false;
  const tick = async () => {
    if (inFlight || !activePaymentPollers.has(orderId)) return;
    inFlight = true;
    try {
      let isPaid = false;
      let isCancelled = false;
      let txnId = '';
      let paidAmount = params.totalAmount;

      if (useDirectPaytm) {
        const pResp = await checkPaytmStatusDirect(paytmStatusUrl, paytmMid, orderId);
        if (String(pResp?.STATUS || '').trim().toUpperCase() === 'TXN_SUCCESS') {
          isPaid = true;
          txnId = String(pResp?.TXNID || '').trim();
          const parsedAmt = parseFloat(String(pResp?.TXNAMOUNT || ''));
          if (Number.isFinite(parsedAmt) && parsedAmt > 0) {
            paidAmount = parsedAmt;
          }
        }
      } else {
        const username = (ramStore.vendor.username || '').trim();
        const raw = await callCentralNode('POST', verifyEndpoint, {
          order_id: orderId,
          ORDERID: orderId,
          username,
          tenant_id: username,
          paytm_mid: paytmMid,
          source: 'vendor_bot',
        });
        const data = raw?.data && isPlainObject(raw.data) ? { ...raw, ...raw.data } : (raw || {});
        const payStatus = String(data.payment_status || '').trim().toUpperCase();
        const ordStatus = String(data.order_status || '').trim().toUpperCase();
        isPaid = data.is_paid === true || data.verified === true || payStatus === 'PAID';
        isCancelled = data.cancelled === true || payStatus === 'CANCELLED' || ordStatus === 'CANCELLED';
        txnId = String(data.txn_id || data.txnid || data.transaction_id || data.utr || '').trim();
        if (Number(data.total_amount) > 0) paidAmount = Number(data.total_amount);
      }

      if (isPaid) {
        stopPaymentVerificationPoller(orderId);
        ramStore.activeCarts.delete(cleanPhone);

        const existing = ramStore.orders.get(orderId);
        upsertOrderRAM(
          {
            order_id: orderId,
            customer_phone: cleanPhone,
            customer_name: existing?.customer_name || params.customerName,
            delivery_address: existing?.delivery_address || params.deliveryAddress,
            total_amount: paidAmount || existing?.total_amount || params.totalAmount,
            payment_mode: 'ONLINE',
            payment_status: 'PAID',
            order_status: 'CONFIRMED',
            source: existing?.source || 'whatsapp',
          },
          true
        );
        upsertCustomerRAM({
          wa_number: cleanPhone,
          current_state: 'ORDER_CONFIRMED',
          payment_status: 'PAID',
          current_order_id: '',
          last_order_details: `Order #${orderId} (₹${paidAmount.toFixed(2)} • CONFIRMED & PAID)`,
        });

        if (markOrderPaidNotifiedOnce(orderId)) {
          sendPaidOrderConfirmationPreview({
            orderId,
            customerPhone: cleanPhone,
            customerName: params.customerName,
            totalAmount: paidAmount,
            txnId,
            deliveryAddress: params.deliveryAddress,
          });
        }

        if (useDirectPaytm) {
          void pushPaymentConfirmedWithRetry(confirmEndpoint, {
            order_id: orderId,
            txn_id: txnId,
            amount: paidAmount,
            paytm_status: 'TXN_SUCCESS',
            customer_phone: cleanPhone,
          });
        }
        return;
      }

      if (isCancelled) {
        stopPaymentVerificationPoller(orderId);
        upsertOrderRAM(
          {
            order_id: orderId,
            customer_phone: cleanPhone,
            payment_status: 'CANCELLED',
            order_status: 'CANCELLED',
          },
          true
        );
      }
    } catch (err: any) {
      console.warn(`⚠️ [PaymentPoller] Verify tick error for Order #${orderId}: ${err?.message || String(err)} (retrying)`);
    } finally {
      inFlight = false;
    }
  };

  const timer = setInterval(() => {
    void tick();
  }, intervalMs);

  const timeout = setTimeout(() => {
    if (!activePaymentPollers.has(orderId)) return;
    stopPaymentVerificationPoller(orderId);
    const timeoutMsg = `Payment verification window for Order #${orderId} timed out. If you already paid, please wait or contact the store with your payment screenshot.`;
    appendChatRAM(cleanPhone, 'assistant', 'outgoing_ai', timeoutMsg);
    pushLiveEvent(cleanPhone, timeoutMsg, 'outgoing_ai');
  }, timeoutMs);

  activePaymentPollers.set(orderId, { timer, timeout });
  console.log(
    useDirectPaytm
      ? `🔄 [PaymentPoller] Started DIRECT Paytm 3s loop for Order #${orderId} (interval=${intervalMs}ms, timeout=${timeoutMs}ms)`
      : `🔄 [PaymentPoller] Started fallback Central 3s loop for Order #${orderId} (interval=${intervalMs}ms, endpoint=${verifyEndpoint})`
  );
}

async function submitWhatsAppOrderToCentral(phone: string, requestedMode: string): Promise<{ reply: string; order?: Order; qr_image_url?: string }> {
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
  let centralTotal = subtotal;
  let upiQrData = '';
  let upiId = ramStore.payment.upi_id || ramStore.payment.merchant_vpa || '';
  let payeeName = ramStore.payment.payee_name || ramStore.vendor.business_name || 'WaMart Store';
  let paytmMid = ramStore.payment.paytm_mid || '';
  let centralMsg = '';

  if (centralNodeURL && vendorApiKey) {
    try {
      const raw = await callCentralNode('POST', '/api/vendor-bot/events/order', submission);
      const data = raw?.data || raw?.order || raw || {};
      if (data.order_id) centralOrderId = String(data.order_id);
      if (data.order_status) centralStatus = String(data.order_status).toUpperCase();
      if (data.payment_status) centralPayStatus = String(data.payment_status).toUpperCase();
      if (data.total_amount !== undefined && data.total_amount !== null) {
        const parsedTotal = parseFlexNumber(data.total_amount, subtotal);
        if (parsedTotal > 0) centralTotal = parsedTotal;
      }
      if (data.upi_qr_data) upiQrData = String(data.upi_qr_data);
      if (data.upi_id) upiId = String(data.upi_id);
      else if (data.merchant_vpa) upiId = String(data.merchant_vpa);
      if (data.payee_name) payeeName = String(data.payee_name);
      if (data.paytm_mid) paytmMid = String(data.paytm_mid);
      if (data.message) centralMsg = String(data.message);
      ramStore.sync.outbound_pushed += 1;
    } catch {
      void pushToCentralAsync('/api/vendor-bot/events/order', submission);
    }
  }

  let localQrDataUrl = '';
  if (mode === 'ONLINE') {
    const upiStr = buildUPIQRString({
      upiQrData,
      upiId,
      payeeName,
      orderId: centralOrderId,
      amount: centralTotal,
    });
    if (upiStr) {
      try {
        const { buffer, mimeType } = await generateLocalBannerQRBuffer(upiStr, ramStore.payment.banner_template);
        localQrDataUrl = `data:${mimeType};base64,${buffer.toString('base64')}`;
        console.log(`🎨 [LocalBannerQR] Generated Order #${centralOrderId} locally (${mimeType}, ${(buffer.length / 1024).toFixed(1)} KB)`);
      } catch (err: any) {
        console.warn(`⚠️ [LocalBannerQR] Failed for Order #${centralOrderId}: ${err?.message || String(err)}`);
      }
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
      total_amount: centralTotal,
      payment_mode: mode,
      payment_status: centralPayStatus,
      order_status: centralStatus,
      payment_link: '',
      payment_qr_url: '',
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

  if (mode === 'ONLINE') {
    // STRICT RULE: Branded Banner QR ONLY — NEVER send any upi:// link or clickable payment URL.
    startPaymentVerificationPoller({
      orderId: centralOrderId,
      customerPhone: cleanPhone,
      customerName: submission.customer_name,
      deliveryAddress: submission.delivery_address,
      totalAmount: centralTotal,
      paytmMid,
    });

    const storeName = ramStore.vendor.business_name || payeeName || 'WaMart Store';
    const caption =
      `🏪 *${storeName}*\n` +
      `🧾 *Order ID:* #${centralOrderId}\n` +
      `💰 *Total Amount:* ₹${centralTotal.toFixed(2)}\n\n` +
      `Please scan & pay using this QR code. Your payment will be verified automatically in a few seconds.`;

    return {
      reply: caption,
      order: saved || undefined,
      qr_image_url: localQrDataUrl || undefined,
    };
  }

  const parts: string[] = [];
  if (centralMsg) {
    parts.push(centralMsg);
  } else {
    parts.push(
      `📦 *Order Request #${centralOrderId} Submitted to Central System*\n\n• Total Amount: *₹${centralTotal.toFixed(2)}*\n• Payment Mode: *Cash on Delivery (COD)*\n• Status: *${centralStatus}*`
    );
  }
  parts.push(
    `✅ Central Brain System se order confirm hote hi aapko final WhatsApp confirmation aur PDF receipt turant mil jayegi!`
  );

  return { reply: parts.join('\n\n'), order: saved || undefined };
}

// =============================================================================
// 4. OCTOPUS AGENTIC AI ENGINE (Groq 3-Model TPD/TPM Pool + Smart Token RAG)
// =============================================================================

const GROQ_ALLOWED_MODELS = [
  'qwen/qwen3.8-27b',
  'openai/gpt-oss-20b',
  'openai/gpt-oss-120b',
] as const;

// Strictly ordered Gemini API priority ladder (applied whenever a Gemini API key is used):
// 1st: gemini-3.5-flash-lite | 2nd: gemini-3.1-flash-lite | 3rd: Gemma 4 26B | 4th: Gemma 4 31B
const GEMINI_PRIORITY_MODELS = [
  'gemini-3.5-flash-lite',
  'gemini-3.1-flash-lite',
  'gemma-4-26b-a4b-it',
  'gemma-4-31b-it',
] as const;

// Strictly active Groq Whisper audio models ONLY for audio transcription
const GROQ_AUDIO_MODELS = [
  'whisper-large-v3-turbo',
  'whisper-large-v3',
] as const;

const MAX_LLM_HISTORY_MESSAGES = 6;

interface GroqSlotState {
  tpmCooldownUntil: number;
  tpdCooldownUntil: number;
  authDisabledUntil: number;
  tokensUsedToday: number;
  requestsToday: number;
  dayUTC: string;
}

interface GeminiSlotState {
  rpmCooldownUntil: number;
  rpdCooldownUntil: number;
  notFoundUntil: number;
  authDisabledUntil: number;
  tokensUsedToday: number;
  requestsToday: number;
  dayUTC: string;
}

interface GroqAudioSlotState {
  rpmCooldownUntil: number;
  rpdCooldownUntil: number;
  authDisabledUntil: number;
  audioSecUsedToday: number;
  requestsToday: number;
  dayUTC: string;
}

const groqSlotStates = new Map<string, GroqSlotState>();
let groqPoolCursor = 0;

const geminiSlotStates = new Map<string, GeminiSlotState>();
let geminiKeyCursor = 0;

const groqAudioSlotStates = new Map<string, GroqAudioSlotState>();
let groqAudioPoolCursor = 0;

function shortKeyHash(apiKey: string): string {
  return crypto.createHash('sha256').update(apiKey.trim()).digest('hex').slice(0, 12);
}

function isGroqProvider(provider: string, apiKey: string): boolean {
  const k = apiKey.trim();
  if (k.startsWith('gsk_')) return true;
  if (k.startsWith('AIzaSy') || k.startsWith('sk-or-') || k.startsWith('sk-')) return false;
  const p = provider.trim().toLowerCase();
  return p === '' || p === 'groq';
}

function isGeminiProvider(provider: string, apiKey: string): boolean {
  const k = apiKey.trim();
  if (k.startsWith('AIzaSy')) return true;
  if (k.startsWith('gsk_') || k.startsWith('sk-or-') || k.startsWith('sk-')) return false;
  return provider.trim().toLowerCase() === 'gemini';
}

function getGeminiSlot(kHash: string, model: string, now: number): GeminiSlotState {
  const dayUTC = new Date(now).toISOString().slice(0, 10);
  const id = `${kHash}|${model}`;
  let st = geminiSlotStates.get(id);
  if (!st) {
    st = { rpmCooldownUntil: 0, rpdCooldownUntil: 0, notFoundUntil: 0, authDisabledUntil: 0, tokensUsedToday: 0, requestsToday: 0, dayUTC };
    geminiSlotStates.set(id, st);
    return st;
  }
  if (st.dayUTC !== dayUTC) {
    st.dayUTC = dayUTC;
    st.tokensUsedToday = 0;
    st.requestsToday = 0;
    st.rpdCooldownUntil = 0;
  }
  return st;
}

function getGroqAudioSlot(kHash: string, model: string, now: number): GroqAudioSlotState {
  const dayUTC = new Date(now).toISOString().slice(0, 10);
  const id = `${kHash}|${model}`;
  let st = groqAudioSlotStates.get(id);
  if (!st) {
    st = { rpmCooldownUntil: 0, rpdCooldownUntil: 0, authDisabledUntil: 0, audioSecUsedToday: 0, requestsToday: 0, dayUTC };
    groqAudioSlotStates.set(id, st);
    return st;
  }
  if (st.dayUTC !== dayUTC) {
    st.dayUTC = dayUTC;
    st.audioSecUsedToday = 0;
    st.requestsToday = 0;
    st.rpdCooldownUntil = 0;
  }
  return st;
}

function getGroqSlot(kHash: string, model: string, now: number): GroqSlotState {
  const dayUTC = new Date(now).toISOString().slice(0, 10);
  const id = `${kHash}|${model}`;
  let st = groqSlotStates.get(id);
  if (!st) {
    st = { tpmCooldownUntil: 0, tpdCooldownUntil: 0, authDisabledUntil: 0, tokensUsedToday: 0, requestsToday: 0, dayUTC };
    groqSlotStates.set(id, st);
    return st;
  }
  if (st.dayUTC !== dayUTC) {
    st.dayUTC = dayUTC;
    st.tokensUsedToday = 0;
    st.requestsToday = 0;
    st.tpdCooldownUntil = 0;
  }
  return st;
}

function parseGroqDurationMs(raw: string | null): number {
  if (!raw) return 0;
  const s = raw.trim();
  if (!s) return 0;
  const asNum = Number(s);
  if (!Number.isNaN(asNum) && asNum > 0) return Math.round(asNum * 1000);
  let total = 0;
  const matches = s.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h)/gi);
  for (const m of matches) {
    const val = Number(m[1]);
    const unit = m[2].toLowerCase();
    if (unit === 'ms') total += val;
    else if (unit === 's') total += val * 1000;
    else if (unit === 'm') total += val * 60_000;
    else if (unit === 'h') total += val * 3_600_000;
  }
  return Math.round(total);
}

function classifyGroq429(headers: Headers, bodyText: string): { isTPD: boolean; cooldownMs: number } {
  const lower = bodyText.toLowerCase();
  const retryAfter = parseGroqDurationMs(headers.get('retry-after'));
  const resetTok = parseGroqDurationMs(headers.get('x-ratelimit-reset-tokens'));
  const resetReq = parseGroqDurationMs(headers.get('x-ratelimit-reset-requests'));

  if (
    lower.includes('tokens per day') ||
    lower.includes('requests per day') ||
    lower.includes('seconds per day') ||
    lower.includes('audio seconds per day') ||
    lower.includes('(tpd)') ||
    lower.includes(' tpd') ||
    lower.includes('(rpd)') ||
    lower.includes(' rpd') ||
    lower.includes('(aspd)') ||
    lower.includes(' aspd') ||
    lower.includes('daily') ||
    retryAfter >= 2 * 3_600_000
  ) {
    return { isTPD: true, cooldownMs: Math.max(retryAfter, 2 * 3_600_000) };
  }
  if (
    lower.includes('seconds per hour') ||
    lower.includes('audio seconds per hour') ||
    lower.includes('(asph)') ||
    lower.includes(' asph') ||
    retryAfter >= 300_000
  ) {
    return { isTPD: false, cooldownMs: (retryAfter || 20 * 60_000) + 2000 };
  }
  const cd = retryAfter || (resetTok > 0 && resetTok < 300_000 ? resetTok : 0) || (resetReq > 0 && resetReq < 300_000 ? resetReq : 0) || 65_000;
  return { isTPD: false, cooldownMs: cd + 2000 };
}

function stripThinkBlocks(text: string): string {
  return text.replace(/<think>[\s\S]*?<\/think>/gi, '').trim();
}

function compactStr(s: string, maxLen: number): string {
  const clean = s.replace(/\n{3,}/g, '\n').trim();
  return clean.length > maxLen ? `${clean.slice(0, maxLen)}…` : clean;
}

function formatRupeeCompact(v: number): string {
  return Number.isInteger(v) ? String(v) : String(Number(v.toFixed(2)));
}

const RAG_STOP_WORDS = new Set([
  'the', 'and', 'for', 'with', 'hai', 'kya', 'karo', 'chahiye', 'dena', 'please',
  'mera', 'meri', 'mere', 'bhai', 'aap', 'tum', 'order', 'mujhe', 'dijiye', 'bhejo',
  'hoga', 'photos', 'photo', 'pic', 'image', 'dikhao', 'price', 'rate', 'kitna',
  'kitne', 'menu', 'list', 'available', 'item', 'items', 'product', 'products',
  'haan', 'han', 'yes', 'nahi', 'hello', 'namaste', 'confirm', 'address', 'delivery',
]);

class ProductAliasMap {
  private realToAlias = new Map<string, string>();
  private aliasToReal = new Map<string, string>();
  private nextNum = 1;

  register(realID: string): string {
    const clean = String(realID || '').trim();
    if (!clean) return '';
    const existing = this.realToAlias.get(clean);
    if (existing) return existing;
    const num = this.nextNum++;
    const alias = `P${num}`;
    this.realToAlias.set(clean, alias);
    this.aliasToReal.set(alias.toLowerCase(), clean);
    this.aliasToReal.set(String(num), clean);
    return alias;
  }

  resolve(idOrAlias: string): string {
    const raw = String(idOrAlias || '').trim();
    if (!raw) return raw;
    const key = raw.replace(/^#/, '').toLowerCase();
    return this.aliasToReal.get(key) || raw;
  }

  count(): number {
    return this.realToAlias.size;
  }
}

function formatCompactProductLine(p: Product, aliases?: ProductAliasMap): string {
  const idLabel = aliases ? aliases.register(p.id) : p.id;
  const unit = p.unit_type && p.unit_type !== 'custom' && p.unit_type !== 'piece' ? `/${p.unit_type}` : '';
  const vars = (p.variants || [])
    .filter((v) => isVariantInStock(v))
    .map((v) => `${v.name}=${formatRupeeCompact(effectivePrice(p, v))}`);
  const varStr = vars.length > 0 ? `|v:${vars.join(',')}` : '';
  return `${idLabel}|${p.name}|₹${formatRupeeCompact(effectivePrice(p))}${unit}${varStr}`;
}

// =============================================================================
// 4-LAYER STATIC HYBRID CATALOG SEARCH (Mirrors Go /whatsapp/smart_catalog.go)
// =============================================================================

const COMMERCE_SYNONYM_GROUPS: string[][] = [
  ['sugar', 'chini', 'cheeni', 'shakkar', 'shakar', 'misri', 'mishri'],
  ['atta', 'aata', 'ata', 'wheat', 'gehu', 'gehun', 'chakki', 'flour'],
  ['maida', 'refined', 'allpurpose'],
  ['besan', 'gram', 'chickpea'],
  ['suji', 'sooji', 'rava', 'semolina'],
  ['poha', 'chura', 'chivda', 'flattened'],
  ['daliya', 'dalia', 'oats', 'porridge'],
  ['sewai', 'sevai', 'vermicelli'],
  ['rice', 'chawal', 'basmati', 'sona', 'kolam'],
  ['dal', 'daal', 'lentil', 'pulses', 'arhar', 'toor', 'tuvar', 'moong', 'mung', 'masoor', 'urad', 'chana', 'rajma', 'chole', 'kabuli'],
  ['mustard', 'sarso', 'sarson', 'kachi', 'ghani'],
  ['oil', 'tel', 'tail', 'refind', 'refined', 'sunflower', 'soyabean', 'groundnut', 'olive'],
  ['ghee', 'ghi', 'clarified'],
  ['butter', 'makkhan', 'makhan'],
  ['milk', 'doodh', 'dudh', 'dairy'],
  ['curd', 'dahi', 'yogurt', 'yoghurt'],
  ['paneer', 'cottage'],
  ['buttermilk', 'chaas', 'chhaach', 'lassi', 'mattha'],
  ['cheese', 'cheez'],
  ['salt', 'namak', 'sendha', 'iodized'],
  ['turmeric', 'haldi'],
  ['chilli', 'chili', 'mirch', 'mirchi', 'lalmirch', 'kashmiri'],
  ['coriander', 'dhaniya', 'dhania'],
  ['cumin', 'jeera', 'jira'],
  ['spice', 'spices', 'masala', 'masale', 'garam'],
  ['pepper', 'kalimirch', 'blackpepper'],
  ['cardamom', 'elaichi', 'ilaychi'],
  ['clove', 'laung', 'long'],
  ['cinnamon', 'dalchini'],
  ['fenugreek', 'methi'],
  ['fennel', 'saunf', 'sonf'],
  ['carom', 'ajwain'],
  ['asafoetida', 'hing', 'heeng'],
  ['ginger', 'adrak', 'adarak'],
  ['garlic', 'lahsun', 'lehsun', 'lasun'],
  ['onion', 'pyaz', 'pyaaz', 'kanda'],
  ['potato', 'aloo', 'alu'],
  ['tomato', 'tamatar'],
  ['lemon', 'nimbu', 'neebu'],
  ['apple', 'seb'],
  ['banana', 'kela'],
  ['mango', 'aam'],
  ['orange', 'santra', 'narangi'],
  ['grapes', 'angoor', 'angur'],
  ['pomegranate', 'anar', 'anaar'],
  ['tea', 'chai', 'chay', 'patti', 'chaipatti'],
  ['coffee', 'kafi', 'nescafe', 'bru'],
  ['biscuit', 'biscuits', 'biskit', 'biskut', 'cookie', 'cookies', 'parle', 'britannia', 'marie', 'goodday', 'oreo'],
  ['namkeen', 'snacks', 'snack', 'bhujia', 'bhujiya', 'mixture', 'sev', 'chips', 'kurkure', 'lays', 'haldiram'],
  ['noodles', 'noodle', 'maggi', 'magi', 'yippee', 'pasta', 'macaroni', 'chowmein'],
  ['chocolate', 'chocolates', 'choco', 'candy', 'toffee', 'kitkat', 'munch', 'perk'],
  ['sweet', 'sweets', 'mithai', 'mithayi', 'laddu', 'ladoo', 'barfi', 'gulab', 'rasgulla', 'soan', 'papdi'],
  ['bread', 'pav', 'bun', 'rusk', 'toast', 'cake', 'bakery', 'muffin', 'cupcake'],
  ['juice', 'colddrink', 'drink', 'soda', 'beverage', 'coke', 'pepsi', 'sprite', 'frooti', 'maaza', 'real', 'tropicana'],
  ['water', 'paani', 'pani', 'bisleri', 'mineral'],
  ['jaggery', 'gur', 'gud'],
  ['honey', 'shahad', 'sahad'],
  ['pickle', 'achar', 'achaar'],
  ['sauce', 'ketchup', 'chutney', 'jam', 'mayo', 'mayonnaise'],
  ['soap', 'sabun', 'saabun', 'bath', 'lifebuoy', 'lux', 'dettol', 'detol', 'dove', 'pears', 'santoor'],
  ['detergent', 'surf', 'washing', 'powder', 'rin', 'tide', 'ariel', 'wheel', 'ghadi', 'kapda'],
  ['shampoo', 'hair', 'baal', 'conditioner', 'clinic', 'sunsilk', 'head', 'pantene', 'dove'],
  ['toothpaste', 'paste', 'manjan', 'colgate', 'colgat', 'pepsodent', 'closeup', 'sensodyne', 'brush', 'toothbrush'],
  ['facewash', 'cream', 'lotion', 'moisturizer', 'talc', 'deodorant', 'deo', 'perfume'],
  ['dishwash', 'bartan', 'vim', 'pril', 'scrubber', 'utensil'],
  ['cleaner', 'floor', 'toilet', 'harpic', 'lizol', 'phenyl', 'finayl', 'colin', 'domex'],
  ['mosquito', 'machhar', 'machar', 'coil', 'allout', 'goodknight', 'mortien', 'hit', 'spray'],
  ['diaper', 'diapers', 'pampers', 'huggies', 'mamy', 'baby', 'wipes'],
  ['sanitary', 'pad', 'pads', 'whisper', 'stayfree', 'sofy'],
  ['pooja', 'puja', 'agarbatti', 'incense', 'dhoop', 'kapoor', 'camphor', 'diya', 'batti', 'roli', 'chandan', 'kumkum'],
  ['cough', 'khansi', 'khasi', 'cold', 'sardi', 'zukham', 'jukham', 'throat', 'gala', 'vicks', 'strepsils', 'honitus', 'inhaler'],
  ['pain', 'dard', 'headache', 'sirdard', 'balm', 'zandu', 'amrutanjan', 'moov', 'volini', 'iodex', 'fever', 'bukhar'],
  ['dryfruit', 'dryfruits', 'almond', 'badam', 'cashew', 'kaju', 'raisin', 'kishmish', 'kismis', 'walnut', 'akhrot', 'pistachio', 'pista', 'makhana', 'dates', 'khajoor'],
  ['peanut', 'peanuts', 'moongfali', 'mungfali', 'singdana'],
  ['coconut', 'nariyal', 'kopra'],
  ['soya', 'soybean', 'soyabean', 'nutrela', 'chunks'],
  ['egg', 'eggs', 'anda', 'ande'],
  ['chicken', 'murga', 'mutton', 'meat', 'fish', 'machli'],
  ['matchbox', 'machis', 'candle', 'mombatti'],
  ['battery', 'cell', 'eveready', 'duracell'],
  ['notebook', 'copy', 'register', 'pen', 'pencil', 'eraser', 'stationery'],
  ['tiffin', 'lunchbox', 'kids', 'bachha', 'bachhe', 'school'],
  ['guest', 'mehmaan', 'mehman', 'party'],
];

const FILLER_ORDER_WORDS = new Set([
  'kilo', 'kg', 'gm', 'gms', 'gram', 'grams', 'ltr', 'litre', 'liter', 'ml',
  'pkt', 'pkts', 'packet', 'packets', 'pack', 'packs', 'pcs', 'pc', 'piece', 'pieces',
  'bottle', 'bottles', 'btl', 'box', 'boxes', 'dozen', 'pouch', 'bag', 'jar',
  'mujhe', 'mujhko', 'chahiye', 'chahie', 'dena', 'dijiye', 'de', 'do', 'bhej', 'bhejo', 'bhejna',
  'add', 'kar', 'karo', 'kardo', 'krdo', 'please', 'pls', 'plz', 'bhai', 'bhaiya', 'sir', 'ji',
  'ka', 'ke', 'ki', 'ko', 'se', 'me', 'mein', 'ek', 'aur', 'and', 'or', 'ya',
  'wala', 'wali', 'wale', 'want', 'need', 'send', 'give', 'get', 'order', 'item', 'items',
  'batao', 'dikhao', 'hai', 'hain', 'hoga', 'kya', 'rate', 'price', 'kitna', 'kitne', 'kaun', 'sa',
  'sabse', 'accha', 'acha', 'best', 'fresh', 'new', 'cart', 'list', 'ye', 'yeh', 'wo', 'woh',
  'mera', 'meri', 'mere', 'apna', 'bhi', 'toh', 'thoda', 'kuch', 'kuchh', 'saman', 'saaman',
]);

const CHECKOUT_ONLY_WORDS = new Set([
  'cod', 'cash', 'online', 'upi', 'paytm', 'gpay', 'phonepe', 'qr', 'pay', 'payment',
  'confirm', 'confirmed', 'proceed', 'checkout', 'place', 'submit', 'yes', 'haan', 'han', 'ha',
  'ok', 'okay', 'theek', 'thik', 'done', 'address', 'addr', 'pincode', 'pin', 'sector', 'road',
  'nagar', 'colony', 'flat', 'house', 'gali', 'mohalla', 'near', 'behind', 'opposite', 'floor',
  'tower', 'block', 'phase', 'village', 'post', 'dist', 'district', 'state', 'city', 'mobile',
  'phone', 'number', 'name', 'naam', 'mera', 'hai', 'deliver', 'delivery', 'kar', 'do', 'karo',
  'kardo', 'bhej', 'bhejo', 'order', 'bill', 'total', 'cancel', 'status', 'track', 'paid',
]);

function phoneticKey(word: string): string {
  const w = word
    .toLowerCase()
    .trim()
    .replace(/chh/g, 'ch')
    .replace(/ph/g, 'f')
    .replace(/sh/g, 's')
    .replace(/kh/g, 'k')
    .replace(/gh/g, 'g')
    .replace(/bh/g, 'b')
    .replace(/dh/g, 'd')
    .replace(/th/g, 't')
    .replace(/ee/g, 'i')
    .replace(/oo/g, 'u')
    .replace(/aa/g, 'a')
    .replace(/ck/g, 'k')
    .replace(/z/g, 'j')
    .replace(/w/g, 'v')
    .replace(/q/g, 'k')
    .replace(/y/g, 'i')
    .replace(/[^a-z0-9]/g, '');
  let out = '';
  for (let i = 0; i < w.length; i++) {
    if (i === 0 || w[i] !== w[i - 1]) out += w[i];
  }
  return out;
}

const SYNONYM_LOOKUP = new Map<string, string[]>();
const PHONETIC_SYN_LOOKUP = new Map<string, string[]>();
for (const group of COMMERCE_SYNONYM_GROUPS) {
  const clean = group.map((w) => w.toLowerCase().trim()).filter(Boolean);
  for (const w of clean) {
    SYNONYM_LOOKUP.set(w, Array.from(new Set([...(SYNONYM_LOOKUP.get(w) || []), ...clean])));
    const pk = phoneticKey(w);
    if (pk.length >= 3) {
      PHONETIC_SYN_LOOKUP.set(pk, Array.from(new Set([...(PHONETIC_SYN_LOOKUP.get(pk) || []), ...clean])));
    }
  }
}

function levenshteinWithin(a: string, b: string, maxDist: number): boolean {
  if (a === b) return true;
  if (Math.abs(a.length - b.length) > maxDist) return false;
  let prev = Array.from({ length: b.length + 1 }, (_, j) => j);
  let curr = new Array<number>(b.length + 1);
  for (let i = 1; i <= a.length; i++) {
    curr[0] = i;
    let rowMin = i;
    for (let j = 1; j <= b.length; j++) {
      const cost = a[i - 1] === b[j - 1] ? 0 : 1;
      const val = Math.min(prev[j] + 1, curr[j - 1] + 1, prev[j - 1] + cost);
      curr[j] = val;
      if (val < rowMin) rowMin = val;
    }
    if (rowMin > maxDist) return false;
    [prev, curr] = [curr, prev];
  }
  return prev[b.length] <= maxDist;
}

interface IndexedProduct {
  p: Product;
  nameLow: string;
  catLow: string;
  descLow: string;
  tagsLow: string;
  variantLow: string;
  nameTokens: string[];
  phonTokens: string[];
}

function tokenizeClean(s: string): string[] {
  return Array.from(
    new Set(
      s
        .toLowerCase()
        .replace(/[^a-z0-9\s]+/g, ' ')
        .split(/\s+/)
        .filter((t) => t.length >= 2)
    )
  );
}

function buildIndexedProducts(products: Product[]): IndexedProduct[] {
  return products.map((p) => {
    const nameLow = p.name.toLowerCase().trim();
    const catLow = (p.category || '').toLowerCase().trim();
    const descLow = (p.description || '').toLowerCase().trim();
    const tagsLow = (p.tags || []).join(' ').toLowerCase().trim();
    const variantLow = (p.variants || []).map((v) => v.name.toLowerCase()).join(' ');
    const nameTokens = tokenizeClean(nameLow);
    const allTokens = tokenizeClean(`${nameLow} ${catLow} ${tagsLow} ${variantLow} ${descLow}`);
    const phonTokens = Array.from(new Set(allTokens.map(phoneticKey).filter((k) => k.length >= 3)));
    return { p, nameLow, catLow, descLow, tagsLow, variantLow, nameTokens, phonTokens };
  });
}

const QTY_UNIT_RE = /\b(\d+(?:\.\d+)?)\s*(kg|kilo|g|gm|gms|gram|grams|l|ltr|litre|liter|ml|pkt|pkts|packet|packets|pack|packs|pcs|pc|piece|pieces|bottle|bottles|btl|box|boxes|dozen|dz|jar|tin|pouch|bag|sachet|strip|strips|x)\b/gi;

function extractSearchTokensAndHint(rawChunk: string): { tokens: string[]; unitHint: string } {
  const low = rawChunk.toLowerCase().trim();
  let unitHint = '';
  const unitMatch = /\b(\d+(?:\.\d+)?)\s*(kg|kilo|g|gm|gms|gram|grams|l|ltr|litre|liter|ml)\b/i.exec(low);
  if (unitMatch) {
    let u = unitMatch[2].toLowerCase();
    if (u === 'kilo') u = 'kg';
    else if (u === 'gm' || u === 'gms' || u === 'gram' || u === 'grams') u = 'g';
    else if (u === 'ltr' || u === 'litre' || u === 'liter') u = 'l';
    unitHint = `${unitMatch[1]}${u}`;
  }
  const cleaned = low.replace(QTY_UNIT_RE, ' ').replace(/[^a-z0-9\s]+/g, ' ');
  const tokens = Array.from(
    new Set(
      cleaned
        .split(/\s+/)
        .filter((f) => f.length >= 2 && !/^\d+$/.test(f) && !FILLER_ORDER_WORDS.has(f) && !RAG_STOP_WORDS.has(f))
    )
  );
  return { tokens, unitHint };
}

function scoreIndexedProduct(ip: IndexedProduct, rawChunk: string, tokens: string[], unitHint: string): number {
  if (tokens.length === 0) return 0;
  let score = 0;
  const chunkLow = rawChunk.toLowerCase().trim();
  if (ip.nameLow.length >= 3 && chunkLow.includes(ip.nameLow)) score += 20;

  let matchedToks = 0;
  for (const tok of tokens) {
    let tokScore = 0;
    const tokPhon = phoneticKey(tok);
    const synList = Array.from(
      new Set([...(SYNONYM_LOOKUP.get(tok) || []), ...(tokPhon.length >= 3 ? PHONETIC_SYN_LOOKUP.get(tokPhon) || [] : [])])
    ).filter((s) => s !== tok);

    if (ip.nameLow.includes(tok)) tokScore = Math.max(tokScore, 10);
    for (const nt of ip.nameTokens) {
      if (nt === tok) tokScore = Math.max(tokScore, 14);
      else if (tok.length >= 4 && nt.startsWith(tok)) tokScore = Math.max(tokScore, 11);
    }
    if (ip.tagsLow && ip.tagsLow.includes(tok)) tokScore = Math.max(tokScore, 10);
    if (ip.catLow.includes(tok)) tokScore = Math.max(tokScore, 6);
    if (ip.descLow.includes(tok)) tokScore = Math.max(tokScore, 3);

    if (tokScore < 12) {
      for (const syn of synList) {
        if (ip.nameLow.includes(syn)) tokScore = Math.max(tokScore, 11);
        else if (ip.tagsLow && ip.tagsLow.includes(syn)) tokScore = Math.max(tokScore, 9);
        else if (ip.catLow.includes(syn)) tokScore = Math.max(tokScore, 6);
        else if (ip.descLow.includes(syn)) tokScore = Math.max(tokScore, 3);
      }
    }

    if (tokScore < 8 && tok.length >= 4) {
      if (tokPhon.length >= 3 && ip.phonTokens.includes(tokPhon)) {
        tokScore = Math.max(tokScore, 9);
      }
      if (tokScore < 7) {
        const maxDist = tok.length >= 6 ? 2 : 1;
        for (const nt of ip.nameTokens) {
          if (nt.length >= 4 && levenshteinWithin(tok, nt, maxDist)) {
            tokScore = Math.max(tokScore, 8);
            break;
          }
        }
      }
    }

    if (tokScore > 0) {
      matchedToks++;
      score += tokScore;
    }
  }

  if (tokens.length >= 2 && matchedToks === tokens.length) score += 8;
  else if (tokens.length >= 2 && matchedToks === 0) return 0;

  if (score > 0 && unitHint) {
    if (ip.nameLow.replace(/\s+/g, '').includes(unitHint) || ip.variantLow.replace(/\s+/g, '').includes(unitHint)) {
      score += 5;
    }
  }
  return score;
}

function splitCustomerItemLines(msg: string): string[] {
  const s = msg.trim().replace(/(?:^|\s)\d{1,2}[.)]\s+/g, '\n');
  if (!s) return [];
  const chunks: string[] = [];
  for (const part of s.split(/[\n\r,;•|]+/)) {
    for (const sub of part.split(/\s+(?:aur|and|\+|&)\s+/i)) {
      const clean = sub
        .trim()
        .replace(/^(?:[-*•]+|\d{1,2}[.)])\s*/, '')
        .replace(/\s+/g, ' ')
        .trim();
      if (clean.length < 2) continue;
      if (extractSearchTokensAndHint(clean).tokens.length === 0) continue;
      chunks.push(clean);
      if (chunks.length >= 35) return chunks;
    }
  }
  return chunks;
}

function isPureCheckoutOrProfileMessage(msg: string, hasCart: boolean): boolean {
  if (!hasCart) return false;
  const toks = tokenizeClean(msg);
  if (toks.length === 0) return true;
  return toks.every((t) => /^\d+$/.test(t) || CHECKOUT_ONLY_WORDS.has(t) || FILLER_ORDER_WORDS.has(t) || RAG_STOP_WORDS.has(t));
}

const EXPLICIT_MENU_WORDS = new Set([
  'menu', 'list', 'catalog', 'catalogue', 'all', 'sab', 'sabhi', 'saare', 'sare',
  'dikhao', 'items', 'products', 'available', 'options', 'kya',
]);

function isExplicitFullMenuRequest(msg: string): boolean {
  const low = msg.toLowerCase();
  if (low.includes('kya kya') || low.includes('all item') || low.includes('sab dikha') || low.includes('full menu')) {
    return true;
  }
  const toks = tokenizeClean(low);
  let hits = 0;
  for (const t of toks) {
    if (t === 'menu' || t === 'catalog' || t === 'catalogue' || t === 'list') return true;
    if (EXPLICIT_MENU_WORDS.has(t)) hits++;
  }
  return hits >= 2;
}

function hasDirectProductMatch(inStockProducts: Product[], query: string): boolean {
  if (inStockProducts.length === 0) return false;
  const { tokens, unitHint } = extractSearchTokensAndHint(query);
  if (tokens.length === 0) return false;
  const indexed = buildIndexedProducts(inStockProducts);
  return indexed.some((ip) => scoreIndexedProduct(ip, query, tokens, unitHint) >= 6);
}

function buildSmartCompactInventory(
  inStockProducts: Product[],
  query: string,
  history: Array<{ role: string; content: string }>,
  cartProductIDs: Set<string>,
  aliases?: ProductAliasMap
): string {
  if (inStockProducts.length === 0) return 'INV: No in-stock items loaded yet.';

  const catBuckets = new Map<string, Product[]>();
  for (const p of inStockProducts) {
    const c = (p.category || 'General').trim();
    const arr = catBuckets.get(c) || [];
    arr.push(p);
    catBuckets.set(c, arr);
  }
  const catSummary = Array.from(catBuckets.entries())
    .map(([c, list]) => `${c}(${list.length})`)
    .join(', ');

  const cartProds = inStockProducts.filter((p) => cartProductIDs.has(p.id));
  if (isPureCheckoutOrProfileMessage(query, cartProds.length > 0)) {
    return `CATS: ${catSummary}\nINV(checkout mode — cart items only):\n${cartProds.map((p) => formatCompactProductLine(p, aliases)).join('\n')}`;
  }

  // Small catalog (<= 15 items): dump ALL compactly ONLY when customer explicitly asks for menu/all items;
  // otherwise let precision matching below send ONLY the 1-6 relevant items!
  if (inStockProducts.length <= 15 && isExplicitFullMenuRequest(query)) {
    return `CATS: ${catSummary}\nINV(id|name|₹price|vars):\n${inStockProducts.map((p) => formatCompactProductLine(p, aliases)).join('\n')}`;
  }

  const indexed = buildIndexedProducts(inStockProducts);
  const itemChunks = splitCustomerItemLines(query);
  if (itemChunks.length >= 2) {
    const clearLines: string[] = [];
    const optionLines: string[] = [];
    const unmatched: string[] = [];
    const includedIds = new Set<string>();
    let totalLines = 0;

    for (const chunk of itemChunks) {
      const { tokens, unitHint } = extractSearchTokensAndHint(chunk);
      if (tokens.length === 0) continue;
      const scored = indexed
        .map((ip) => ({ ip, score: scoreIndexedProduct(ip, chunk, tokens, unitHint) }))
        .filter((x) => x.score >= 6)
        .sort((a, b) => b.score - a.score);

      if (scored.length === 0) {
        unmatched.push(chunk);
        continue;
      }

      const top = scored[0];
      const isClear = top.score >= 12 && (scored.length === 1 || top.score >= scored[1].score + 6);
      if (isClear) {
        clearLines.push(`• ${chunk} => ${formatCompactProductLine(top.ip.p, aliases)}`);
        if (!includedIds.has(top.ip.p.id)) {
          includedIds.add(top.ip.p.id);
          totalLines++;
        }
      } else {
        const limit = scored.length >= 3 && scored[2].score >= top.score - 2 && totalLines < 40 ? 3 : Math.min(2, scored.length);
        const opts: string[] = [];
        for (let k = 0; k < limit; k++) {
          if (totalLines >= 42 && !includedIds.has(scored[k].ip.p.id)) break;
          opts.push(formatCompactProductLine(scored[k].ip.p, aliases));
          if (!includedIds.has(scored[k].ip.p.id)) {
            includedIds.add(scored[k].ip.p.id);
            totalLines++;
          }
        }
        if (opts.length > 0) optionLines.push(`• ${chunk} => ${opts.join(' OR ')}`);
      }
    }

    if (clearLines.length > 0 || optionLines.length > 0) {
      const parts = [`CATS: ${catSummary}`, `MULTI_ITEM_MATCH(${clearLines.length + optionLines.length} lines matched from ${inStockProducts.length} in-stock):`];
      if (clearLines.length > 0) parts.push(`CLEAR_MATCH(ready to add):\n${clearLines.join('\n')}`);
      if (optionLines.length > 0) parts.push(`VARIETY_OPTIONS(ask/pick best):\n${optionLines.join('\n')}`);
      if (unmatched.length > 0) parts.push(`UNMATCHED(use search_catalog if indirect need, else inform): ${unmatched.join(', ')}`);
      const extraCart = cartProds.filter((cp) => !includedIds.has(cp.id)).map((p) => formatCompactProductLine(p, aliases));
      if (extraCart.length > 0) parts.push(`IN_CART_REF:\n${extraCart.join('\n')}`);
      return parts.join('\n');
    }
  }

  const prevUserMsgs = history.filter((m) => m.role === 'user').slice(-2).map((m) => m.content);
  const combinedQuery = `${query} ${prevUserMsgs.join(' ')}`;
  const { tokens, unitHint } = extractSearchTokensAndHint(combinedQuery);

  const selectedIds = new Set<string>();
  const selected: Product[] = [];
  for (const cp of cartProds) {
    selectedIds.add(cp.id);
    selected.push(cp);
  }

  const scored =
    tokens.length > 0
      ? indexed
          .filter((ip) => !selectedIds.has(ip.p.id))
          .map((ip) => ({ ip, score: scoreIndexedProduct(ip, combinedQuery, tokens, unitHint) }))
          .filter((x) => x.score >= 5)
          .sort((a, b) => b.score - a.score)
      : [];

  for (const item of scored) {
    if (selected.length >= 10) break;
    selectedIds.add(item.ip.p.id);
    selected.push(item.ip.p);
  }

  if (selected.length === 0 || (scored.length === 0 && isExplicitFullMenuRequest(query))) {
    const maxShow = isExplicitFullMenuRequest(query) ? 10 : 5;
    const cats = Array.from(catBuckets.values());
    let round = 0;
    while (selected.length < maxShow) {
      let added = false;
      for (const bucket of cats) {
        if (round < bucket.length) {
          const p = bucket[round];
          if (!selectedIds.has(p.id)) {
            selectedIds.add(p.id);
            selected.push(p);
            added = true;
            if (selected.length >= maxShow) break;
          }
        }
      }
      if (!added) break;
      round++;
    }
  }

  const hint =
    scored.length === 0 && tokens.length > 0
      ? '\nHINT: 0 direct hits. If indirect need, call search_catalog with keywords/CATS.'
      : '';
  return `CATS: ${catSummary}\nINV(${selected.length}/${inStockProducts.length}; id|name|₹price|vars):\n${selected.map((p) => formatCompactProductLine(p, aliases)).join('\n')}${hint}`;
}

function executeSearchCatalogHand(inStockProducts: Product[], queries: string[], categories: string[], aliases?: ProductAliasMap): string {
  if (inStockProducts.length === 0) return 'SEARCH_RESULTS: No in-stock products available.';
  const indexed = buildIndexedProducts(inStockProducts);
  const cleanCats = categories.map((c) => c.toLowerCase().trim()).filter(Boolean);
  const combinedQ = queries.join(' ');
  const { tokens, unitHint } = extractSearchTokensAndHint(combinedQ);

  const results = indexed
    .map((ip) => {
      let sc = tokens.length > 0 ? scoreIndexedProduct(ip, combinedQ, tokens, unitHint) : 0;
      for (const wantCat of cleanCats) {
        if (ip.catLow === wantCat || ip.catLow.includes(wantCat) || wantCat.includes(ip.catLow)) sc += 12;
      }
      return { p: ip.p, score: sc };
    })
    .filter((x) => x.score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, 12);

  if (results.length === 0) {
    return `SEARCH_RESULTS(0 matches): Not in stock. Politely inform & suggest CATS.`;
  }
  return `SEARCH_RESULTS(${results.length}; id|name|₹price|vars):\n${results.map((r) => formatCompactProductLine(r.p, aliases)).join('\n')}`;
}

// =============================================================================
// MICRO-CAPSULE ADAPTIVE CONTEXT ENGINE (MCACE v3 — Mirrors Go micro_capsule.go)
// =============================================================================

type TurnCapsule = 'GREETING' | 'CHITCHAT_INFO' | 'PAYMENT_PENDING' | 'CHECKOUT' | 'SHOPPING';

const PURE_GREETING_WORDS = new Set([
  'hi', 'hii', 'hiii', 'hiiii', 'hey', 'heyy', 'hello', 'helo', 'hlo', 'hlw', 'hola', 'yo',
  'namaste', 'namaskar', 'namastey', 'pranam', 'pranaam', 'ram', 'radhe', 'jai', 'shri',
  'salam', 'salaam', 'assalam', 'assalamu', 'alaikum', 'walaikum', 'sat', 'akal',
  'good', 'morning', 'afternoon', 'evening', 'night', 'gm', 'gn',
  'ji', 'sir', 'madam', 'mam', 'maam', 'bhai', 'bhaiya', 'bro', 'didi', 'boss', 'friend', 'dear',
  'shop', 'store', 'kaise', 'kese', 'ho', 'hai', 'hain', 'aap', 'tum', 'kya', 'haal', 'hal', 'chal', 'chaal',
  'start', 'test', 'how', 'are', 'you', 'there', 'anyone', 'koi',
]);

const PURE_ACK_WORDS = new Set([
  'ok', 'okay', 'okk', 'okkk', 'k', 'kk', 'ji', 'haan', 'han', 'ha', 'yes', 'yep', 'yup',
  'hmm', 'hm', 'hmmm', 'thik', 'theek', 'thk', 'sahi', 'achha', 'accha', 'acha', 'nice', 'great',
  'good', 'cool', 'awesome', 'super', 'perfect', 'fine', 'done', 'sure',
  'thanks', 'thank', 'thankyou', 'ty', 'thx', 'tq', 'dhanyawad', 'dhanyavaad', 'shukriya', 'welcome',
  'bye', 'byee', 'alvida', 'later', 'baad', 'me', 'mein', 'sir', 'bhai', 'bhaiya', 'bro', 'hai',
]);

const STORE_INFO_KEYWORDS = new Set([
  'timing', 'timings', 'time', 'hours', 'open', 'close', 'closed', 'khula', 'khulta', 'khulti', 'band', 'kab',
  'delivery', 'charge', 'charges', 'fee', 'free', 'radius', 'minimum', 'min', 'policy', 'return', 'refund', 'exchange',
  'website', 'site', 'link', 'url', 'mall', 'app', 'location', 'address', 'kahan', 'kaha', 'where', 'dukaan', 'dukan',
  'owner', 'malik', 'manager', 'human', 'agent', 'call', 'contact', 'number', 'support', 'help', 'complaint', 'shikayat',
  'damaged', 'kharab', 'wrong', 'galat', 'cod', 'cash', 'upi', 'online',
]);

const SHOPPING_FORCE_WORDS = new Set([
  'menu', 'list', 'catalog', 'catalogue', 'item', 'items', 'product', 'products', 'saman', 'saaman',
  'rate', 'rates', 'price', 'prices', 'kitna', 'kitne', 'bhav', 'daam', 'dam',
  'photo', 'photos', 'pic', 'pics', 'image', 'images', 'dikhao', 'dikha',
  'chahiye', 'chaiye', 'bhejo', 'bhej', 'dena', 'dedo', 'lana', 'kharidna', 'buy', 'order', 'cart', 'add', 'remove', 'hatao',
  'available', 'stock', 'milega', 'milta', 'variety', 'brand', 'brands', 'pack', 'packet', 'kg', 'gm', 'litre',
  'guest', 'mehmaan', 'mehman', 'party', 'snacks', 'nashta', 'breakfast', 'khansi', 'cough', 'dard', 'bukhar', 'fever', 'pooja', 'puja',
]);

const capsuleTelemetry = {
  totalTurns: 0,
  totalEstInTok: 0,
  perCapsule: {} as Record<string, number>,
};

function classifyTurnCapsule(query: string, currentState: string, hasCart: boolean, hasProdMatch: boolean): TurnCapsule {
  const toks = tokenizeClean(query);
  if (hasProdMatch) return 'SHOPPING';
  const hasShopWord = toks.some((t) => SHOPPING_FORCE_WORDS.has(t));
  const hasDigit = toks.some((t) => /\d/.test(t));

  if (currentState === 'PAYMENT_PENDING' && !hasShopWord) return 'PAYMENT_PENDING';
  if (toks.length > 0 && toks.length <= 5 && !hasDigit && !hasShopWord && toks.every((t) => PURE_GREETING_WORDS.has(t))) {
    return 'GREETING';
  }
  if (toks.length === 0 && query.trim() !== '' && !hasCart) return 'GREETING';
  if (hasCart && isPureCheckoutOrProfileMessage(query, true)) return 'CHECKOUT';
  if (toks.length > 0 && toks.length <= 4 && !hasDigit && !hasShopWord && toks.every((t) => PURE_ACK_WORDS.has(t))) {
    return hasCart ? 'CHECKOUT' : 'CHITCHAT_INFO';
  }
  if (!hasShopWord && !hasCart && toks.length > 0 && toks.length <= 12 && toks.some((t) => STORE_INFO_KEYWORDS.has(t))) {
    return 'CHITCHAT_INFO';
  }
  return 'SHOPPING';
}

function stripDecorForPastTurn(s: string): string {
  return s
    .replace(/\r/g, '')
    .split('\n')
    .map((ln) => ln.trim().replace(/^[>•\-*\s]+/, ''))
    .filter(Boolean)
    .join(' | ')
    .replace(/[*_~]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

function buildGreetingHighlightsTS(products: Product[]): { catsLine: string; topSample: string } {
  if (products.length === 0) return { catsLine: '', topSample: '' };
  const catCounts = new Map<string, number>();
  const catFirst = new Map<string, Product>();
  for (const p of products) {
    const c = (p.category || 'General').trim();
    if (!catCounts.has(c)) catFirst.set(c, p);
    catCounts.set(c, (catCounts.get(c) || 0) + 1);
  }
  const catEntries = Array.from(catCounts.entries());
  const catsLine =
    catEntries.length > 10
      ? `${catEntries.slice(0, 10).map(([c, n]) => `${c}(${n})`).join(', ')}, +${catEntries.length - 10} more`
      : catEntries.map(([c, n]) => `${c}(${n})`).join(', ');
  const picks: string[] = [];
  for (const p of catFirst.values()) {
    if (picks.length >= 3) break;
    picks.push(`${p.name.trim()} ₹${formatRupeeCompact(effectivePrice(p))}`);
  }
  for (const p of products) {
    if (picks.length >= 3) break;
    const entry = `${p.name.trim()} ₹${formatRupeeCompact(effectivePrice(p))}`;
    if (!picks.includes(entry)) picks.push(entry);
  }
  return { catsLine, topSample: picks.join(' | ') };
}

function resolveActionAliases(actions: Array<{ hand: string; payload?: any }>, aliases: ProductAliasMap) {
  if (!aliases || aliases.count() === 0) return;
  for (const act of actions) {
    if (act.payload && Array.isArray(act.payload.cart_items)) {
      for (const item of act.payload.cart_items) {
        if (item && typeof item.id === 'string') {
          item.id = aliases.resolve(item.id);
        }
      }
    }
  }
}

async function callGroqPool(
  groqKeys: string[],
  systemPrompt: string,
  history: Array<{ role: string; content: string }>,
  temperature: number
): Promise<{ text: string; keyIdx: number; model: string } | null> {
  const now = Date.now();
  const matrix: Array<{ keyIdx: number; apiKey: string; kHash: string; model: string }> = [];
  for (const model of GROQ_ALLOWED_MODELS) {
    for (let kIdx = 0; kIdx < groqKeys.length; kIdx++) {
      matrix.push({ keyIdx: kIdx, apiKey: groqKeys[kIdx], kHash: shortKeyHash(groqKeys[kIdx]), model });
    }
  }
  if (matrix.length === 0) return null;

  const start = groqPoolCursor % matrix.length;
  groqPoolCursor++;
  const rotated = [...matrix.slice(start), ...matrix.slice(0, start)];

  const ready: Array<(typeof matrix)[number] & { tokensDay: number }> = [];
  const tpmDelayed: Array<(typeof matrix)[number] & { tpmUntil: number }> = [];

  for (const slot of rotated) {
    const st = getGroqSlot(slot.kHash, slot.model, now);
    if (now < st.authDisabledUntil || now < st.tpdCooldownUntil) continue;
    if (now < st.tpmCooldownUntil) tpmDelayed.push({ ...slot, tpmUntil: st.tpmCooldownUntil });
    else ready.push({ ...slot, tokensDay: st.tokensUsedToday });
  }

  ready.sort((a, b) => Math.floor(a.tokensDay / 5000) - Math.floor(b.tokensDay / 5000));
  tpmDelayed.sort((a, b) => a.tpmUntil - b.tpmUntil);

  const messages = [{ role: 'system', content: systemPrompt }, ...history];

  const attemptSlot = async (slot: (typeof matrix)[number]) => {
    const execReq = async (minimal: boolean) => {
      const payload: Record<string, any> = {
        model: slot.model,
        temperature,
        max_completion_tokens: 600,
        messages,
      };
      if (!minimal) {
        payload.response_format = { type: 'json_object' };
        if (slot.model.startsWith('openai/gpt-oss')) {
          payload.reasoning_effort = 'low';
        }
      }
      return fetch('https://api.groq.com/openai/v1/chat/completions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${slot.apiKey}` },
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(18000),
      });
    };

    try {
      let r = await execReq(false);
      if (r.status === 400) {
        r = await execReq(true);
      }
      const bodyText = await r.text();
      const st = getGroqSlot(slot.kHash, slot.model, Date.now());
      if (r.ok) {
        const json: any = JSON.parse(bodyText);
        const content = stripThinkBlocks(String(json?.choices?.[0]?.message?.content || ''));
        if (content) {
          st.tokensUsedToday += Number(json?.usage?.total_tokens || 0);
          st.requestsToday += 1;
          st.tpmCooldownUntil = 0;
          const remTok = Number(r.headers.get('x-ratelimit-remaining-tokens') || '-1');
          if (!Number.isNaN(remTok) && remTok >= 0 && remTok < 800) {
            st.tpmCooldownUntil = Date.now() + Math.min(parseGroqDurationMs(r.headers.get('x-ratelimit-reset-tokens')) || 15_000, 75_000);
          }
          return { text: content, keyIdx: slot.keyIdx, model: slot.model };
        }
      } else if (r.status === 429) {
        const { isTPD, cooldownMs } = classifyGroq429(r.headers, bodyText);
        if (isTPD) {
          const utcMidnight = new Date();
          utcMidnight.setUTCHours(24, 1, 0, 0);
          st.tpdCooldownUntil = Math.min(utcMidnight.getTime(), Date.now() + cooldownMs);
          console.warn(`🚫 [Groq TPD Exhausted] Key #${slot.keyIdx + 1} | model=${slot.model} -> rotating`);
        } else {
          st.tpmCooldownUntil = Date.now() + cooldownMs;
          console.warn(`⚡ [Groq TPM Limit] Key #${slot.keyIdx + 1} | model=${slot.model} -> switching model/key`);
        }
      } else if (r.status === 401 || r.status === 403) {
        for (const m of GROQ_ALLOWED_MODELS) {
          getGroqSlot(slot.kHash, m, Date.now()).authDisabledUntil = Date.now() + 30 * 60_000;
        }
      } else {
        st.tpmCooldownUntil = Date.now() + 45_000;
      }
    } catch {
      getGroqSlot(slot.kHash, slot.model, Date.now()).tpmCooldownUntil = Date.now() + 45_000;
    }
    return null;
  };

  for (const slot of ready) {
    const res = await attemptSlot(slot);
    if (res) return res;
  }
  if (tpmDelayed.length > 0) {
    const best = tpmDelayed[0];
    const waitMs = best.tpmUntil - Date.now();
    if (waitMs > 0 && waitMs <= 3000) {
      await new Promise((resolve) => setTimeout(resolve, waitMs));
    }
    const res = await attemptSlot(best);
    if (res) return res;
  }
  return null;
}

function buildGeminiSDKContents(
  systemPrompt: string,
  history: Array<{ role: string; content: string }>,
  foldSystemIntoUser: boolean
): Array<{ role: 'user' | 'model'; parts: Array<{ text: string }> }> {
  const merged: Array<{ role: 'user' | 'model'; text: string }> = [];
  for (const m of history) {
    const txt = String(m.content || '').trim();
    if (!txt) continue;
    const role: 'user' | 'model' = m.role === 'assistant' || m.role === 'model' ? 'model' : 'user';
    if (merged.length > 0 && merged[merged.length - 1].role === role) {
      merged[merged.length - 1].text += `\n${txt}`;
    } else {
      merged.push({ role, text: txt });
    }
  }
  if (merged.length === 0) {
    merged.push({ role: 'user', text: 'Hello' });
  }
  if (foldSystemIntoUser && systemPrompt.trim()) {
    const prefix = `[SYSTEM INSTRUCTIONS]\n${systemPrompt.trim()}\n\n[CONVERSATION]\n`;
    if (merged[0].role === 'user') {
      merged[0].text = prefix + merged[0].text;
    } else {
      merged.unshift({ role: 'user', text: `${prefix}Please follow the system instructions.` });
    }
  }
  return merged.map((item) => ({ role: item.role, parts: [{ text: item.text }] }));
}

function classifyGeminiError(err: any): { status: number; isDaily: boolean; cooldownMs: number } {
  const status = Number(err?.status || err?.statusCode || err?.code || 0);
  const msg = String(err?.message || err || '').toLowerCase();
  const is429 = status === 429 || msg.includes('resource_exhausted') || msg.includes('429') || msg.includes('quota');
  if (is429) {
    const isDaily =
      msg.includes('perday') ||
      msg.includes('per_day') ||
      msg.includes('per day') ||
      msg.includes('requests per day') ||
      msg.includes('tokens per day') ||
      msg.includes('daily') ||
      msg.includes('rpd') ||
      msg.includes('tpd');
    return { status: 429, isDaily, cooldownMs: isDaily ? 2 * 3_600_000 : 65_000 };
  }
  if (status === 404 || msg.includes('not_found') || msg.includes('404')) {
    return { status: 404, isDaily: false, cooldownMs: 6 * 3_600_000 };
  }
  if (status === 401 || status === 403 || msg.includes('api_key_invalid') || msg.includes('permission_denied')) {
    return { status: status || 403, isDaily: false, cooldownMs: 30 * 60_000 };
  }
  return { status: status || 500, isDaily: false, cooldownMs: 45_000 };
}

// Strict Priority Gemini Pool via @google/genai SDK:
// 1st: gemini-3.5-flash-lite -> 2nd: gemini-3.1-flash-lite -> 3rd: gemma-4-26b-a4b-it -> 4th: gemma-4-31b-it
// Spreads requests across all active Gemini keys within each priority tier and spills over automatically on RPM/RPD limits.
async function callGeminiPool(
  geminiKeys: string[],
  systemPrompt: string,
  history: Array<{ role: string; content: string }>,
  temperature: number
): Promise<{ text: string; keyIdx: number; model: string } | null> {
  if (geminiKeys.length === 0) return null;
  const now = Date.now();
  const keyCount = geminiKeys.length;
  const keyStart = geminiKeyCursor % keyCount;
  geminiKeyCursor++;

  type Candidate = { priority: number; keyIdx: number; apiKey: string; kHash: string; model: string; requestsDay: number; rpmUntil: number };
  const ready: Candidate[] = [];
  const rpmDelayed: Candidate[] = [];

  for (let prio = 0; prio < GEMINI_PRIORITY_MODELS.length; prio++) {
    const model = GEMINI_PRIORITY_MODELS[prio];
    const tierReady: Candidate[] = [];
    for (let offset = 0; offset < keyCount; offset++) {
      const keyIdx = (keyStart + offset) % keyCount;
      const apiKey = geminiKeys[keyIdx];
      const kHash = shortKeyHash(apiKey);
      const st = getGeminiSlot(kHash, model, now);
      if (now < st.authDisabledUntil || now < st.rpdCooldownUntil || now < st.notFoundUntil) continue;
      const slot: Candidate = { priority: prio, keyIdx, apiKey, kHash, model, requestsDay: st.requestsToday, rpmUntil: st.rpmCooldownUntil };
      if (now < st.rpmCooldownUntil) rpmDelayed.push(slot);
      else tierReady.push(slot);
    }
    tierReady.sort((a, b) => Math.floor(a.requestsDay / 5) - Math.floor(b.requestsDay / 5));
    ready.push(...tierReady);
  }

  rpmDelayed.sort((a, b) => (a.rpmUntil !== b.rpmUntil ? a.rpmUntil - b.rpmUntil : a.priority - b.priority));

  const attemptSlot = async (slot: Candidate) => {
    const isGemma = slot.model.startsWith('gemma-');
    const ai = new GoogleGenAI({
      apiKey: slot.apiKey,
      httpOptions: { headers: { 'User-Agent': 'aistudio-build' } },
    });

    const execCall = async (compatMode: boolean) => {
      const foldSystem = isGemma || compatMode;
      const contents = buildGeminiSDKContents(systemPrompt, history, foldSystem);
      const config: Record<string, any> = {
        temperature,
        maxOutputTokens: 600,
      };
      if (!foldSystem) {
        config.systemInstruction = systemPrompt;
        config.responseMimeType = 'application/json';
      }
      return ai.models.generateContent({
        model: slot.model,
        contents,
        config,
      });
    };

    try {
      let response;
      try {
        response = await execCall(false);
      } catch (firstErr: any) {
        const errStatus = Number(firstErr?.status || firstErr?.statusCode || 0);
        if (!isGemma && errStatus === 400) {
          response = await execCall(true);
        } else {
          throw firstErr;
        }
      }
      const content = stripThinkBlocks(String(response?.text || '').trim());
      if (content) {
        const st = getGeminiSlot(slot.kHash, slot.model, Date.now());
        st.tokensUsedToday += Number(response?.usageMetadata?.totalTokenCount || 0);
        st.requestsToday += 1;
        st.rpmCooldownUntil = 0;
        return { text: content, keyIdx: slot.keyIdx, model: slot.model };
      }
    } catch (err: any) {
      const st = getGeminiSlot(slot.kHash, slot.model, Date.now());
      const info = classifyGeminiError(err);
      if (info.status === 429) {
        if (info.isDaily) {
          const utcMidnight = new Date();
          utcMidnight.setUTCHours(24, 1, 0, 0);
          st.rpdCooldownUntil = Math.min(utcMidnight.getTime(), Date.now() + info.cooldownMs);
          console.warn(`🚫 [Gemini Daily Limit] Priority #${slot.priority + 1} (${slot.model}) | Key #${slot.keyIdx + 1} -> switching`);
        } else {
          st.rpmCooldownUntil = Date.now() + info.cooldownMs;
          console.warn(`⚡ [Gemini RPM/TPM Limit] Priority #${slot.priority + 1} (${slot.model}) | Key #${slot.keyIdx + 1} -> switching`);
        }
      } else if (info.status === 404) {
        st.notFoundUntil = Date.now() + info.cooldownMs;
        console.warn(`⚠️ [Gemini 404] Priority #${slot.priority + 1} (${slot.model}) -> cooling down 6h & switching`);
      } else if (info.status === 401 || info.status === 403) {
        for (const m of GEMINI_PRIORITY_MODELS) {
          getGeminiSlot(slot.kHash, m, Date.now()).authDisabledUntil = Date.now() + info.cooldownMs;
        }
      } else {
        st.rpmCooldownUntil = Date.now() + info.cooldownMs;
      }
    }
    return null;
  };

  for (const slot of ready) {
    const res = await attemptSlot(slot);
    if (res) return res;
  }
  if (rpmDelayed.length > 0) {
    const best = rpmDelayed[0];
    const waitMs = best.rpmUntil - Date.now();
    if (waitMs > 0 && waitMs <= 3000) {
      await new Promise((resolve) => setTimeout(resolve, waitMs));
    }
    const res = await attemptSlot(best);
    if (res) return res;
  }
  return null;
}

// Groq Whisper Audio Transcription Pool (STRICTLY whisper-large-v3-turbo & whisper-large-v3 ONLY)
// Uses ALL active Groq keys × BOTH Whisper models with balanced daily limit tracking & instant 429 switching.
async function transcribeVoiceNoteGroqPool(
  audioBuffer: Buffer,
  durationSec = 10
): Promise<{ text: string; model: string; keyIdx: number } | null> {
  if (!audioBuffer || audioBuffer.length === 0) return null;
  const groqKeys = allAIKeys().filter((k) => k.trim().startsWith('gsk_'));
  if (groqKeys.length === 0) return null;

  const now = Date.now();
  const matrix: Array<{ keyIdx: number; apiKey: string; kHash: string; model: string }> = [];
  for (const model of GROQ_AUDIO_MODELS) {
    for (let kIdx = 0; kIdx < groqKeys.length; kIdx++) {
      matrix.push({ keyIdx: kIdx, apiKey: groqKeys[kIdx], kHash: shortKeyHash(groqKeys[kIdx]), model });
    }
  }
  const start = groqAudioPoolCursor % matrix.length;
  groqAudioPoolCursor++;
  const rotated = [...matrix.slice(start), ...matrix.slice(0, start)];

  const ready: Array<(typeof matrix)[number] & { audioSecDay: number }> = [];
  const rpmDelayed: Array<(typeof matrix)[number] & { rpmUntil: number }> = [];
  for (const slot of rotated) {
    const st = getGroqAudioSlot(slot.kHash, slot.model, now);
    if (now < st.authDisabledUntil || now < st.rpdCooldownUntil) continue;
    if (now < st.rpmCooldownUntil) rpmDelayed.push({ ...slot, rpmUntil: st.rpmCooldownUntil });
    else ready.push({ ...slot, audioSecDay: st.audioSecUsedToday });
  }
  ready.sort((a, b) => Math.floor(a.audioSecDay / 120) - Math.floor(b.audioSecDay / 120));
  rpmDelayed.sort((a, b) => a.rpmUntil - b.rpmUntil);

  const attemptAudioSlot = async (slot: (typeof matrix)[number]) => {
    try {
      const form = new FormData();
      form.append('file', new Blob([new Uint8Array(audioBuffer)], { type: 'audio/ogg' }), 'voice.ogg');
      form.append('model', slot.model);
      const r = await fetch('https://api.groq.com/openai/v1/audio/transcriptions', {
        method: 'POST',
        headers: { Authorization: `Bearer ${slot.apiKey}` },
        body: form,
        signal: AbortSignal.timeout(15000),
      });
      const bodyText = await r.text();
      const st = getGroqAudioSlot(slot.kHash, slot.model, Date.now());
      if (r.ok) {
        const json: any = JSON.parse(bodyText);
        const text = String(json?.text || '').trim();
        if (text) {
          st.audioSecUsedToday += Math.max(1, durationSec);
          st.requestsToday += 1;
          st.rpmCooldownUntil = 0;
          return { text, model: slot.model, keyIdx: slot.keyIdx };
        }
      } else if (r.status === 429) {
        const { isTPD, cooldownMs } = classifyGroq429(r.headers, bodyText);
        if (isTPD) {
          const utcMidnight = new Date();
          utcMidnight.setUTCHours(24, 1, 0, 0);
          st.rpdCooldownUntil = Math.min(utcMidnight.getTime(), Date.now() + cooldownMs);
        } else {
          st.rpmCooldownUntil = Date.now() + cooldownMs;
        }
      } else if (r.status === 401 || r.status === 403) {
        for (const m of GROQ_AUDIO_MODELS) {
          getGroqAudioSlot(slot.kHash, m, Date.now()).authDisabledUntil = Date.now() + 30 * 60_000;
        }
      } else {
        st.rpmCooldownUntil = Date.now() + 45_000;
      }
    } catch {
      getGroqAudioSlot(slot.kHash, slot.model, Date.now()).rpmCooldownUntil = Date.now() + 45_000;
    }
    return null;
  };

  for (const slot of ready) {
    const res = await attemptAudioSlot(slot);
    if (res) return res;
  }
  if (rpmDelayed.length > 0) {
    const best = rpmDelayed[0];
    const waitMs = best.rpmUntil - Date.now();
    if (waitMs > 0 && waitMs <= 3000) {
      await new Promise((resolve) => setTimeout(resolve, waitMs));
    }
    const res = await attemptAudioSlot(best);
    if (res) return res;
  }
  return null;
}
void transcribeVoiceNoteGroqPool;

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

  // 3. Micro-Capsule Adaptive Context Engine (MCACE v3) + Multi-Key Model Pools
  if (candidateKeys.length > 0) {
    const allChat = ramStore.chats.get(cleanPhone) || [];
    const hasCart = currentCart.length > 0;
    const currentState = hasCart ? 'CART_ACTIVE' : 'BROWSING';
    const hasProdMatch = hasDirectProductMatch(inStockProducts, query);
    const capsule = classifyTurnCapsule(query, currentState, hasCart, hasProdMatch);
    const maxHist =
      capsule === 'GREETING' ? 1 : capsule === 'CHITCHAT_INFO' ? 2 : capsule === 'PAYMENT_PENDING' ? 3 : capsule === 'CHECKOUT' ? 4 : hasCart ? MAX_LLM_HISTORY_MESSAGES : 4;

    const rawHistory = allChat.slice(-(Math.max(1, maxHist) - 1));
    const llmHistory: Array<{ role: string; content: string }> =
      maxHist === 1
        ? []
        : rawHistory.map((m) => {
            const role = m.role === 'assistant' || m.direction.startsWith('outgoing') ? 'assistant' : 'user';
            const content = role === 'assistant' ? compactStr(stripDecorForPastTurn(m.text), 90) : compactStr(m.text, 120);
            return { role, content };
          });
    if (llmHistory.length === 0 || llmHistory[llmHistory.length - 1].content !== compactStr(query, 500)) {
      llmHistory.push({ role: 'user', content: compactStr(query, 500) });
    }
    const finalHistory = llmHistory.slice(-maxHist);

    const aliases = new ProductAliasMap();
    const isFirstTurn = allChat.length <= 1;
    const firstTip = isFirstTurn ? '\nFirst chat: warmly ask them to type their full request in one message.' : '';
    const vendorNote = ramStore.ai.system_prompt ? `\nNOTE: ${compactStr(ramStore.ai.system_prompt, 180)}` : '';
    const bName = ramStore.vendor.business_name || 'Vendor Store';
    const bType = ramStore.vendor.business_type || 'Retail Store';
    const lang = ramStore.vendor.language || 'Hinglish';
    const payMode = ramStore.vendor.allow_cod ? 'COD+ONLINE' : 'ONLINE_ONLY';
    const storeLine = `Hrs:${ramStore.vendor.operating_hours}|ETA:${ramStore.vendor.estimated_delivery_time}|Min₹${formatRupeeCompact(ramStore.vendor.min_order_amount)}|Pay:${payMode}`;

    const cartTotal = currentCart.reduce((sum, it) => sum + it.line_total, 0);
    const cartLine = hasCart
      ? `CART(₹${formatRupeeCompact(cartTotal)}): ${currentCart
          .map((c) => `${aliases.register(c.product_id)} ${c.product_name}${c.selected_variant ? `[${c.selected_variant}]` : ''} x${c.qty}=₹${formatRupeeCompact(c.line_total)}`)
          .join('; ')}`
      : 'CART:empty';
    const profParts: string[] = [];
    if (cust?.full_name) profParts.push(`Name=${cust.full_name}`);
    if (cust?.delivery_address) profParts.push(`Addr=${compactStr(cust.delivery_address, 110)}`);
    if (cust?.contact_phone) profParts.push(`Ph=${cust.contact_phone}`);
    const custLine = profParts.length > 0 ? `\nCUST:${profParts.join('|')}` : '';

    let basePrompt = '';
    let invBlock = '';

    if (capsule === 'GREETING') {
      const { catsLine, topSample } = buildGreetingHighlightsTS(inStockProducts);
      basePrompt = `Warm, lively WhatsApp shopkeeper for "${bName}" (${bType}). Reply in ${lang}.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: natural human shopkeeper tone, *bold* store/items/*₹prices*, friendly emojis (✨🛍️🙏💛), crisp bullets. Welcome warmly, highlight categories & popular picks, and offer photos/help.${firstTip}${vendorNote}${cust?.full_name ? `\nCUST: ${cust.full_name}` : ''}${hasCart ? `\nCART: ${currentCart.length} items (₹${formatRupeeCompact(cartTotal)})` : ''}${catsLine ? `\nCATS: ${catsLine}` : ''}${topSample ? `\nTOP: ${topSample}` : ''}`;
    } else if (capsule === 'CHITCHAT_INFO') {
      const { catsLine } = buildGreetingHighlightsTS(inStockProducts);
      const policies = compactStr(ramStore.vendor.store_policies || '', 140);
      basePrompt = `Warm WhatsApp shopkeeper for "${bName}" (${bType}). Reply in ${lang}.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: friendly human tone, *bold* key info, natural emojis. quote_reply:true only for specific questions/complaints.
HANDS(optional): generate_store_link:{} | escalate_to_human:{}
Never ask UTR/screenshots; online payments verify automatically.${firstTip}${policies ? `\nPOLICY: ${policies}` : ''}${vendorNote}
STORE: ${storeLine}${cust?.full_name ? `\nCUST: ${cust.full_name}` : ''}${catsLine ? `\nCATS: ${catsLine}` : ''}`;
    } else if (capsule === 'CHECKOUT') {
      basePrompt = `Warm WhatsApp shopkeeper for "${bName}". Reply in ${lang}.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: crisp human tone, *bold* items/*₹totals*, bullets, emojis. Quote (> ) only for total/payment box.
HANDS(only needed):
• extract_profile:{"profile":{"name":"","address":"","contact":""}}
• submit_order:{"payment_mode":"COD"|"ONLINE"} (ONLY when cart+address+mode ready)
• clear_cart:{} | cancel_order:{} | escalate_to_human:{}
RULES: Before submit_order show *Order Summary* (items, total, address) & ask *Online* 💳 or *COD* 💵 (if allowed). Never ask UTR/screenshots.${vendorNote}
STORE: ${storeLine}
STATE:${currentState} | ${cartLine}${custLine}`;
    } else {
      const cartProductIDs = new Set(currentCart.map((c) => c.product_id));
      const checkoutHands = hasCart ? '\n• submit_order:{"payment_mode":"COD"|"ONLINE"} | clear_cart:{} | cancel_order:{}' : '';
      const checkoutRule = hasCart ? ' Before submit_order show *Order Summary* & ask Online/COD.' : '';
      basePrompt = `Warm, lively WhatsApp shopkeeper for "${bName}" (${bType}). Reply in ${lang}.
OUTPUT RAW JSON ONLY: {"ai_reply_text":"...","quote_reply":false,"actions":[]}
Style: natural human tone, *bold* items/*₹prices*/variants, emojis (✨🛍️🔥💰🙌), "• " bullets. Quote (> ) only for highlight boxes. quote_reply:true only for direct question/price/item/complaint.
HANDS(only needed; [] for chat):
• search_catalog:{"queries":["kw"],"categories":["Cat"]} (ONLY if user's need/item missing from INV; use CATS)
• send_catalog_images:{"category":"item|cat|all"} (when photos asked)
• update_cart:{"cart_items":[{"id":"P1","variant":"","qty":1}]} (use EXACT short ID P1/P2 from INV/CLEAR_MATCH; add CLEAR_MATCH immediately, ask brand/size for VARIETY_OPTIONS)
• extract_profile:{"profile":{"name":"","address":"","contact":""}} | generate_store_link:{} | escalate_to_human:{}${checkoutHands}
RULES: Quote/add ONLY in-stock items from INV/CLEAR_MATCH/VARIETY_OPTIONS/SEARCH_RESULTS. Never ask UTR/screenshots.${checkoutRule}${firstTip}${vendorNote}
STORE: ${storeLine}
STATE:${currentState} | ${cartLine}${custLine}`;
      invBlock = buildSmartCompactInventory(inStockProducts, query, finalHistory, cartProductIDs, aliases);
    }

    const systemPrompt = invBlock ? `${basePrompt}\n\n${invBlock}` : basePrompt;
    const estInTok = Math.ceil((systemPrompt.length + finalHistory.reduce((s, h) => s + h.content.length + 12, 0)) / 3.6);
    capsuleTelemetry.totalTurns += 1;
    capsuleTelemetry.totalEstInTok += estInTok;
    capsuleTelemetry.perCapsule[capsule] = (capsuleTelemetry.perCapsule[capsule] || 0) + 1;
    console.log(`🧠 [Micro-Capsule] +${cleanPhone} | capsule=${capsule} | ~${estInTok} est_in_tok | hist=${finalHistory.length} | aliases=${aliases.count()}`);

    const groqKeys = candidateKeys.filter((k) => isGroqProvider(provider, k));
    const geminiKeys = candidateKeys.filter((k) => isGeminiProvider(provider, k));
    const otherKeys = candidateKeys.filter((k) => !isGroqProvider(provider, k) && !isGeminiProvider(provider, k));
    const primaryKey = String(overrideApiKey || ramStore.ai.api_key || '').trim();
    const preferGemini = provider === 'gemini' || primaryKey.startsWith('AIzaSy');

    const parseJsonReply = (
      rawText: string,
      engine: string,
      keyIdx: number
    ): {
      ai_reply_text: string;
      quote_reply: boolean;
      actions: Array<{ hand: string; payload?: any }>;
      engine: string;
      key_used_index: number;
    } | null => {
      const match = rawText.match(/\{[\s\S]*\}/);
      if (!match) return null;
      try {
        const parsed = JSON.parse(match[0]);
        return {
          ai_reply_text: String(parsed.ai_reply_text || parsed.reply_to_customer || '').trim(),
          quote_reply: Boolean(parsed.quote_reply),
          actions: Array.isArray(parsed.actions) ? parsed.actions : [],
          engine,
          key_used_index: keyIdx,
        };
      } catch {
        return null;
      }
    };

    const invokeLLMOnce = async (promptToSend: string): Promise<{
      ai_reply_text: string;
      quote_reply: boolean;
      actions: Array<{ hand: string; payload?: any }>;
      engine: string;
      key_used_index: number;
    } | null> => {
      const tryGroq = async () => {
        if (groqKeys.length === 0) return null;
        const groqRes = await callGroqPool(groqKeys, promptToSend, finalHistory, ramStore.ai.temperature || 0.72);
        if (groqRes?.text) {
          return parseJsonReply(groqRes.text, `groq:${groqRes.model} (key #${groqRes.keyIdx + 1}/${groqKeys.length})`, groqRes.keyIdx);
        }
        return null;
      };

      const tryGemini = async () => {
        if (geminiKeys.length === 0) return null;
        const gemRes = await callGeminiPool(geminiKeys, promptToSend, finalHistory, ramStore.ai.temperature || 0.72);
        if (gemRes?.text) {
          return parseJsonReply(gemRes.text, `gemini:${gemRes.model} (key #${gemRes.keyIdx + 1}/${geminiKeys.length})`, gemRes.keyIdx);
        }
        return null;
      };

      if (preferGemini) {
        const gRes = await tryGemini();
        if (gRes) return gRes;
        const qRes = await tryGroq();
        if (qRes) return qRes;
      } else {
        const qRes = await tryGroq();
        if (qRes) return qRes;
        const gRes = await tryGemini();
        if (gRes) return gRes;
      }

      for (let idx = 0; idx < otherKeys.length; idx++) {
        const apiKey = otherKeys[idx];
        try {
          const endpoint =
            provider === 'deepseek'
              ? 'https://api.deepseek.com/chat/completions'
              : provider === 'openrouter' || apiKey.startsWith('sk-or-')
                ? 'https://openrouter.ai/api/v1/chat/completions'
                : 'https://api.openai.com/v1/chat/completions';
          const model =
            provider === 'deepseek'
              ? ramStore.ai.model || 'deepseek-chat'
              : provider === 'openrouter' || apiKey.startsWith('sk-or-')
                ? ramStore.ai.model || 'meta-llama/llama-3.1-8b-instruct:free'
                : ramStore.ai.model || 'gpt-4o-mini';
          const r = await fetch(endpoint, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${apiKey}` },
            body: JSON.stringify({
              model,
              temperature: ramStore.ai.temperature || 0.72,
              max_tokens: 600,
              response_format: { type: 'json_object' },
              messages: [{ role: 'system', content: promptToSend }, ...finalHistory],
            }),
            signal: AbortSignal.timeout(15000),
          });
          if (!r.ok) throw new Error(`HTTP ${r.status}`);
          const json: any = await r.json();
          const rawText = stripThinkBlocks(json?.choices?.[0]?.message?.content || '');
          if (rawText) {
            const parsed = parseJsonReply(rawText, `${provider} (key #${idx + 1}/${otherKeys.length})`, idx);
            if (parsed) return parsed;
          }
        } catch (err) {
          console.warn(`⚠️ [Multi-Key AI Fallback] Key #${idx + 1}/${otherKeys.length} failed, rotating...`, err);
        }
      }
      return null;
    };

    const firstPass = await invokeLLMOnce(systemPrompt);
    if (firstPass) {
      resolveActionAliases(firstPass.actions, aliases);
      // Layer 4: On-Demand `search_catalog` Hand interception (STRICTLY max 1 follow-up pass!)
      const searchActions = firstPass.actions.filter((a) =>
        ['search_catalog', 'find_products', 'browse_category', 'lookup_catalog', 'search_products'].includes(
          String(a.hand || '').toLowerCase()
        )
      );
      if (searchActions.length > 0) {
        const queries: string[] = [];
        const categories: string[] = [];
        for (const sa of searchActions) {
          const p = sa.payload || {};
          if (Array.isArray(p.queries)) queries.push(...p.queries.map((x: any) => String(x || '').trim()).filter(Boolean));
          if (typeof p.query === 'string' && p.query.trim()) queries.push(p.query.trim());
          if (Array.isArray(p.categories)) categories.push(...p.categories.map((x: any) => String(x || '').trim()).filter(Boolean));
          if (typeof p.category === 'string' && p.category.trim()) categories.push(p.category.trim());
        }
        if (queries.length > 0 || categories.length > 0) {
          const lookupBlock = executeSearchCatalogHand(inStockProducts, queries, categories, aliases);
          // Smart Pass-2 Swap: replace Pass-1 INV cleanly with SEARCH_RESULTS
          const secondPass = await invokeLLMOnce(
            `${basePrompt}\n\n${lookupBlock}\n\nFINAL PASS: Answer warmly using SEARCH_RESULTS above (use exact P1/P2 IDs). Do NOT call search_catalog again.`
          );
          if (secondPass) {
            resolveActionAliases(secondPass.actions, aliases);
            secondPass.actions = secondPass.actions.filter(
              (a) => !['search_catalog', 'find_products', 'browse_category', 'lookup_catalog', 'search_products'].includes(String(a.hand || '').toLowerCase())
            );
            return secondPass;
          }
        }
        firstPass.actions = firstPass.actions.filter(
          (a) => !['search_catalog', 'find_products', 'browse_category', 'lookup_catalog', 'search_products'].includes(String(a.hand || '').toLowerCase())
        );
      }
      return firstPass;
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
    bot_mode: ramStore.botMode,
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
    bot_mode: ramStore.botMode,
    sync: {
      ...ramStore.sync,
      bot_public_url: botPublicURL,
      retry_buffer_depth: retryBuffer.length,
    },
    vendor: ramStore.vendor,
    vendor_management: {
      settings: ramStore.vendorManagement.settings,
      owner_payment_gateway: ramStore.vendorManagement.owner_payment_gateway,
      subscription_plans_count: ramStore.vendorManagement.subscription_plans.length,
      managed_vendors_count: ramStore.vendorManagement.managed_vendors.length,
    },
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
      store_policies: ramStore.ai.store_policies,
      gemini_priority_models: [...GEMINI_PRIORITY_MODELS],
      groq_text_models: [...GROQ_ALLOWED_MODELS],
      groq_audio_models: [...GROQ_AUDIO_MODELS],
      capsule_stats: {
        architecture: 'micro_capsule_v3',
        total_turns: capsuleTelemetry.totalTurns,
        avg_est_in_tok: capsuleTelemetry.totalTurns > 0 ? Math.round(capsuleTelemetry.totalEstInTok / capsuleTelemetry.totalTurns) : 0,
        capsule_dispatch: { ...capsuleTelemetry.perCapsule },
      },
    },
    payment: {
      enabled:
        ramStore.payment.enabled ||
        Boolean(ramStore.payment.paytm_mid || ramStore.payment.upi_id || ramStore.payment.merchant_vpa || ramStore.payment.gateway),
      gateway: ramStore.payment.gateway,
      upi_id: ramStore.payment.upi_id || ramStore.payment.merchant_vpa,
      merchant_vpa: ramStore.payment.merchant_vpa || ramStore.payment.upi_id,
      paytm_mid: ramStore.payment.paytm_mid,
      payee_name: ramStore.payment.payee_name || ramStore.vendor.business_name,
      merchant_phone: ramStore.payment.merchant_phone,
      local_qr_generation: ramStore.payment.local_qr_generation,
      local_paytm_verify: ramStore.payment.local_paytm_verify,
      paytm_status_url: ramStore.payment.paytm_status_url || DEFAULT_PAYTM_STATUS_URL,
      payment_confirm_endpoint: ramStore.payment.payment_confirm_endpoint || DEFAULT_PAYMENT_CONFIRM_ENDPOINT,
      qr_only_mode: ramStore.payment.qr_only_mode,
      send_payment_link: ramStore.payment.send_payment_link,
      verify_endpoint: ramStore.payment.verify_endpoint || DEFAULT_VERIFY_ENDPOINT,
      verify_poll_interval_ms: ramStore.payment.verify_poll_interval_ms || DEFAULT_VERIFY_POLL_INTERVAL_MS,
      verify_poll_timeout_ms: ramStore.payment.verify_poll_timeout_ms || DEFAULT_VERIFY_POLL_TIMEOUT_MS,
      banner_template: ramStore.payment.banner_template,
      banner_cache: getBlankBannerCacheStats(),
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
  const rawMode = firstDefined(data, 'bot_mode') ?? (isPlainObject(vRaw) ? firstDefined(vRaw, 'bot_mode') : undefined);
  if (rawMode !== undefined && String(rawMode).trim()) {
    ramStore.botMode = normalizeBotMode(rawMode);
  }
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
  const topBannerFallback = parseFlexString(firstDefined(data, 'universal_qr_banner_url', 'qr_banner_url')).trim();
  if (payRaw !== undefined || topBannerFallback) {
    if (payRaw !== undefined && !isPlainObject(payRaw)) {
      warnings.push('payment section is not a JSON object');
    } else {
      const mergedPay = isPlainObject(payRaw) ? { ...payRaw } : {};
      if (topBannerFallback && !mergedPay.universal_qr_banner_url) {
        mergedPay.universal_qr_banner_url = topBannerFallback;
      }
      applyPaymentSection(mergedPay);
    }
  }
  const vmRaw = firstDefined(data, 'vendor_management');
  if (vmRaw !== undefined) {
    if (isPlainObject(vmRaw)) applyVendorManagementSection(vmRaw);
    else warnings.push('vendor_management section is not a JSON object');
  }

  markInbound('vendor');
  const resp: Record<string, any> = {
    status: 'success',
    bot_mode: ramStore.botMode,
    message: 'Vendor, AI, Payment & Vendor Management configuration synced to RAM',
  };
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
  const isPaidOrConfirmedEvent = ev === 'order.paid' || ev === 'order.confirmed';
  const isFinalConfirm =
    isPaidOrConfirmedEvent ||
    ev === 'order.created' ||
    (statusChanged && (saved?.order_status === 'CONFIRMED' || saved?.payment_status === 'PAID'));

  if (saved && saved.order_id) {
    if (
      isPaidOrConfirmedEvent ||
      saved.payment_status === 'PAID' ||
      saved.order_status === 'CONFIRMED' ||
      saved.payment_status === 'CANCELLED' ||
      saved.order_status === 'CANCELLED'
    ) {
      stopPaymentVerificationPoller(saved.order_id);
    }
  }

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
    if (isPaidOrConfirmedEvent || saved.payment_status === 'PAID') {
      // STRICT DEDUPLICATION: Only notify if notify_whatsapp === true AND order_id not yet in notifiedPaidOrders.
      if (notifyOpt === true && markOrderPaidNotifiedOnce(saved.order_id)) {
        const txnId = parseFlexString(
          firstDefined(req.body, 'txn_id', 'txnid', 'transaction_id', 'utr') ??
            (isPlainObject(rawOrder) ? firstDefined(rawOrder, 'txn_id', 'txnid', 'transaction_id', 'utr') : undefined)
        );
        sendPaidOrderConfirmationPreview({
          orderId: saved.order_id,
          customerPhone: saved.customer_phone,
          customerName: saved.customer_name,
          totalAmount: saved.total_amount,
          txnId,
          deliveryAddress: saved.delivery_address,
        });
      }
    } else {
      const shouldNotify = notifyOpt !== undefined ? notifyOpt : statusChanged || isFinalConfirm;
      if (shouldNotify) {
        const msg = isFinalConfirm
          ? `🎉 *Order Confirmed by Central Mall!* (#${saved.order_id})\n\n• Total: *₹${saved.total_amount.toFixed(2)}* (${saved.payment_mode} · ${saved.payment_status})\n• Order confirmed and ready for processing!`
          : buildDeliveryStatusAlert(saved);
        appendChatRAM(saved.customer_phone, 'assistant', 'outgoing_ai', msg);
        pushLiveEvent(saved.customer_phone, msg, 'outgoing_ai');
      }
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
  const phone = normalizePhone(parseFlexString(req.body.phone));
  const message = parseFlexString(req.body.message).trim();
  const imageUrl = parseFlexString(req.body.image_url).trim();
  if (!phone || (!message && !imageUrl)) {
    res.status(400).json({ status: 'error', message: 'phone and message (or image_url) are required' });
    return;
  }
  const previewText = imageUrl ? (message ? `${message}\n🖼️ ${imageUrl}` : `🖼️ ${imageUrl}`) : message;
  appendChatRAM(phone, 'assistant', 'outgoing_ai', previewText);
  pushLiveEvent(phone, previewText, 'outgoing_ai');
  markInbound('send_message');
  res.json({ status: 'success', bot_mode: ramStore.botMode, message: 'Queued for WhatsApp delivery' });
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

const MESSAGE_READ_DELAY_MS = 1000;
const SPLIT_MESSAGE_PEACEFUL_NOTICE =
  '🙏 *Kripya dhyan dein ji!* 😊\n\nApni kisi bhi baat ko *ek sath likh kar bhejein*, unhe tukdo me karke na bhejein — taaki main aapko *fast aur behtar* reply de saku! ✨💛';
const FIRST_TURN_SINGLE_MESSAGE_TIP =
  '\n\n💡 _Ek chhoti si request:_ Koshish karein ki apni poori baat *ek sath ek hi message me* likh kar bhejein (tukdo me nahi), taaki main aapko aur jaldi & best help kar saku! 😊✨';

const previewCustomerInFlight = new Set<string>();
const previewSplitNoticeSent = new Set<string>();

function hasPriorAssistantReplyPreview(phone: string): boolean {
  const list = ramStore.chats.get(phone) || [];
  return list.some((m) => m.role === 'assistant' || m.direction === 'outgoing_ai' || m.direction === 'outgoing_manual');
}

function replyHasSingleMsgTip(text: string): boolean {
  const low = text.toLowerCase();
  return (
    low.includes('tukdo') ||
    low.includes('ek sath') ||
    low.includes('ek saath') ||
    low.includes('ek hi message') ||
    low.includes('one message') ||
    low.includes('single message')
  );
}

const pendingAdminPlanPickers = new Map<string, { plans: SubscriptionPlan[]; expiresAt: number }>();

function buildPlanRenewalUPIString(upiVpa: string, payeeName: string, orderId: string, amount: number): string {
  const vpa = (upiVpa || '').trim();
  if (!vpa) return '';
  const pn = encodeURIComponent((payeeName || '').trim() || 'WaMart');
  const tr = encodeURIComponent(orderId.trim());
  return `upi://pay?pa=${vpa}&pn=${pn}&am=${amount.toFixed(2)}&cu=INR&tr=${tr}&tn=WaMart+Plan+Renewal`;
}

function buildAdminVendorSystemPromptPreview(vendor: ManagedVendor | undefined, vm: VendorManagementState): string {
  const lines: string[] = [];
  const custom = (ramStore.ai.system_prompt || '').trim();
  if (custom) {
    lines.push(custom);
  } else {
    lines.push(
      'You are WaMart Vendor Support Manager — the official WhatsApp support assistant for registered WaMart store owners/vendors.',
      'Help vendors with subscription plans, renewals, WhatsApp bot setup, UPI payments, WaMart Digital Mall listing, and store configuration.',
      'NEVER act as a grocery store, NEVER take shopping orders, NEVER discuss shopping carts or customer checkout.',
      'Keep replies concise, friendly, accurate, and formatted cleanly for WhatsApp.'
    );
  }
  const policies = (ramStore.ai.store_policies || ramStore.vendor.store_policies || '').trim();
  if (policies) {
    lines.push('', 'PLATFORM POLICIES:', policies);
  }
  if (vm.settings.support_escalation_phone) {
    lines.push('', `Support Escalation Phone: ${vm.settings.support_escalation_phone}`);
  }
  if (vendor) {
    lines.push(
      '',
      'MATCHED VENDOR LIVE CONTEXT:',
      `- Store Name: ${vendor.store_name || vendor.username}`,
      `- Username: ${vendor.username}`,
      `- Owner Phone: ${vendor.owner_phone}`,
      `- Active Plan: ${vendor.plan_name || 'N/A'} (ID: ${vendor.plan_id || 'N/A'})`,
      `- Valid Until: ${vendor.plan_valid_until || 'N/A'}`,
      `- Days Remaining: ${vendor.days_remaining}`,
      `- Expired: ${vendor.is_plan_expired ? 'Yes' : 'No'}`,
      `- Bot Connected: ${vendor.bot_connected ? 'Yes' : 'No'}`,
      `- UPI ID: ${vendor.upi_id || 'Not Configured'}`,
      `- Store URL: ${vendor.store_url || 'N/A'}`
    );
  } else {
    lines.push('', 'SENDER STATUS: Phone not matched in managed_vendors yet.');
  }
  if (vm.subscription_plans.length > 0) {
    lines.push('', 'AVAILABLE WAMART SUBSCRIPTION PLANS:');
    for (const p of vm.subscription_plans) {
      lines.push(
        `- ${p.name} (id=${p.id}): Monthly ₹${p.monthly_price.toFixed(0)}, Yearly ₹${p.yearly_price.toFixed(0)}, Validity ${p.validity_days || 30} days`
      );
    }
  }
  if (vm.settings.vendor_self_service_commands) {
    lines.push('', 'Quick self-service commands available: my plan, renew, store status, mall status, help.');
  }
  return lines.join('\n');
}

async function processAdminVendorTextPreview(
  phone: string,
  query: string,
  overrideProvider?: string,
  overrideApiKey?: string
): Promise<{ reply: string; engine: string; keyIdx: number; qrDataUrl?: string; action?: string; payload?: Record<string, any> }> {
  const vm = ramStore.vendorManagement;
  const vendor = lookupManagedVendorByPhone(phone);
  const low = query.trim().toLowerCase().replace(/^\/+/, '').replace(/\s+/g, ' ');

  const generateRenewalReply = async (plan: SubscriptionPlan, billing: 'monthly' | 'yearly') => {
    const gw = vm.owner_payment_gateway;
    const amount = billing === 'yearly' && plan.yearly_price > 0 ? plan.yearly_price : plan.monthly_price > 0 ? plan.monthly_price : plan.yearly_price;
    const validity = billing === 'yearly' ? 365 : plan.validity_days || 30;
    const orderId = `WMP-${Date.now()}-${Math.floor(100 + Math.random() * 900)}`;
    const upiStr = buildPlanRenewalUPIString(gw.upi_vpa, gw.payee_name || 'WaMart', orderId, amount);
    let qrDataUrl = '';
    try {
      const { buffer, mimeType } = await generateLocalBannerQRBuffer(upiStr, ramStore.payment.banner_template);
      qrDataUrl = `data:${mimeType};base64,${buffer.toString('base64')}`;
    } catch (err: any) {
      console.warn(`⚠️ [AdminRenewalQR] Preview QR generation warning: ${err?.message || String(err)}`);
    }
    const storeLabel = vendor?.store_name || vendor?.username || 'Your Store';
    const reply = [
      `🔄 *WaMart Plan Renewal QR*`,
      ``,
      `🏪 Store: *${storeLabel}*`,
      `📦 Plan: *${plan.name}* (${billing === 'yearly' ? 'Yearly' : 'Monthly'} · ${validity} days)`,
      `💰 Amount: *₹${amount.toFixed(2)}*`,
      `🧾 Ref ID: \`${orderId}\``,
      ``,
      `Scan the QR image using any UPI app (GPay / PhonePe / Paytm) to complete renewal.`,
      gw.paytm_mid ? `✅ Auto-verification is active — your plan upgrades automatically on payment.` : `📩 Once paid, reply here or share your UTR for instant activation.`,
    ].join('\n');
    return {
      reply,
      engine: 'admin-vendor-command',
      keyIdx: -1,
      qrDataUrl,
      action: 'plan_renewal_qr',
      payload: { order_id: orderId, plan_id: plan.id, plan_name: plan.name, amount, billing },
    };
  };

  if (vm.settings.vendor_self_service_commands) {
    // Check pending multi-plan picker selection first
    const pending = pendingAdminPlanPickers.get(phone);
    if (pending && Date.now() < pending.expiresAt) {
      const idxNum = Number(low.replace(/\D/g, ''));
      const wantsYearly = low.includes('year') || low.includes('annual');
      let chosen: SubscriptionPlan | undefined;
      if (idxNum >= 1 && idxNum <= pending.plans.length) {
        chosen = pending.plans[idxNum - 1];
      } else {
        chosen = pending.plans.find((p) => p.name.toLowerCase() === low || p.id.toLowerCase() === low);
      }
      if (chosen) {
        pendingAdminPlanPickers.delete(phone);
        return generateRenewalReply(chosen, wantsYearly ? 'yearly' : 'monthly');
      }
    } else if (pending) {
      pendingAdminPlanPickers.delete(phone);
    }

    if (low === 'help' || low === 'menu') {
      return {
        reply: [
          `👋 *WaMart Vendor Support Menu*`,
          ``,
          `Reply with any quick command:`,
          `• *my plan* — View active plan & expiry`,
          `• *renew* — Renew or upgrade your plan via UPI QR`,
          `• *store status* — Check store URL, UPI & bot status`,
          `• *mall status* — Check WaMart Digital Mall eligibility`,
          ``,
          `Or simply type any question and our Vendor Support Manager will assist you!`,
        ].join('\n'),
        engine: 'admin-vendor-command',
        keyIdx: -1,
        action: 'help',
      };
    }

    if (low === 'my plan' || low === 'plan') {
      if (!vendor) {
        return {
          reply: `ℹ️ We couldn't find a registered WaMart vendor store linked to this WhatsApp number.${vm.settings.support_escalation_phone ? `\n📞 Support: ${vm.settings.support_escalation_phone}` : ''}`,
          engine: 'admin-vendor-command',
          keyIdx: -1,
          action: 'my_plan',
        };
      }
      const statusText = vendor.is_plan_expired || vendor.days_remaining <= 0 ? '❌ Expired' : '✅ Active';
      return {
        reply: [
          `📋 *Your WaMart Plan Details*`,
          ``,
          `🏪 Store: *${vendor.store_name || vendor.username}*`,
          `📦 Plan: *${vendor.plan_name || 'Standard'}*`,
          `📅 Valid Until: *${vendor.plan_valid_until || 'N/A'}*`,
          `⏳ Days Remaining: *${Math.max(0, vendor.days_remaining)} day(s)*`,
          `🔖 Status: *${statusText}*`,
          ``,
          `Reply *renew* anytime to renew or upgrade.`,
        ].join('\n'),
        engine: 'admin-vendor-command',
        keyIdx: -1,
        action: 'my_plan',
      };
    }

    if (low === 'store status' || low === 'status') {
      if (!vendor) {
        return {
          reply: `ℹ️ No WaMart store is mapped to this phone number yet.`,
          engine: 'admin-vendor-command',
          keyIdx: -1,
          action: 'store_status',
        };
      }
      return {
        reply: [
          `🏪 *Store Status — ${vendor.store_name || vendor.username}*`,
          ``,
          `🌐 Store URL: ${vendor.store_url || 'Not published yet'}`,
          `💳 UPI VPA: ${vendor.upi_id ? `✅ Configured (${vendor.upi_id})` : '⚠️ Not configured'}`,
          `🤖 WhatsApp Bot: ${vendor.bot_connected ? '✅ Connected' : '❌ Disconnected'}`,
          `📦 Plan: *${vendor.plan_name || 'N/A'}* (${vendor.is_plan_expired ? '❌ Expired' : `✅ Active · ${vendor.days_remaining}d left`})`,
        ].join('\n'),
        engine: 'admin-vendor-command',
        keyIdx: -1,
        action: 'store_status',
      };
    }

    if (low === 'mall status') {
      if (!vendor) {
        return {
          reply: `ℹ️ Register your WaMart store first to check WaMart Digital Mall listing eligibility.`,
          engine: 'admin-vendor-command',
          keyIdx: -1,
          action: 'mall_status',
        };
      }
      const eligible = !vendor.is_plan_expired && vendor.days_remaining > 0 && Boolean(vendor.store_url);
      return {
        reply: [
          `🏬 *WaMart Digital Mall Status*`,
          ``,
          `🏪 Store: *${vendor.store_name || vendor.username}*`,
          `📦 Plan: *${vendor.plan_name || 'N/A'}* (${vendor.is_plan_expired ? '❌ Expired' : '✅ Active'})`,
          `✅ Mall Eligibility: *${eligible ? 'Eligible & Active' : 'Action Required (Active Plan + Store URL needed)'}*`,
        ].join('\n'),
        engine: 'admin-vendor-command',
        keyIdx: -1,
        action: 'mall_status',
      };
    }

    if (low === 'renew' || low === 'upgrade' || low.startsWith('renew ') || low.startsWith('upgrade ')) {
      if (!vm.settings.whatsapp_plan_renewal_qr || !vm.owner_payment_gateway.upi_vpa) {
        return {
          reply: `ℹ️ Instant WhatsApp QR plan renewal is currently disabled. Please renew from your WaMart Vendor Dashboard${vm.settings.support_escalation_phone ? ` or contact ${vm.settings.support_escalation_phone}` : ''}.`,
          engine: 'admin-vendor-command',
          keyIdx: -1,
          action: 'renew_unavailable',
        };
      }
      const paidPlans = vm.subscription_plans.filter((p) => p.monthly_price > 0 || p.yearly_price > 0);
      if (paidPlans.length === 0) {
        return {
          reply: `ℹ️ No paid subscription plans are currently configured.`,
          engine: 'admin-vendor-command',
          keyIdx: -1,
          action: 'renew_no_plans',
        };
      }
      const wantsYearly = low.includes('year') || low.includes('annual');
      const explicitPlan = paidPlans.find(
        (p) => low.includes(p.name.toLowerCase()) || (p.id && low.includes(p.id.toLowerCase()))
      );
      if (explicitPlan) {
        return generateRenewalReply(explicitPlan, wantsYearly ? 'yearly' : 'monthly');
      }
      if (paidPlans.length === 1) {
        return generateRenewalReply(paidPlans[0], wantsYearly ? 'yearly' : 'monthly');
      }
      pendingAdminPlanPickers.set(phone, { plans: paidPlans, expiresAt: Date.now() + 10 * 60_000 });
      const listLines = paidPlans.map(
        (p, idx) =>
          `${idx + 1}. *${p.name}* — ₹${p.monthly_price.toFixed(0)}/mo${p.yearly_price > 0 ? ` | ₹${p.yearly_price.toFixed(0)}/yr` : ''}`
      );
      return {
        reply: [
          `🔄 *Choose a WaMart Plan to Renew / Upgrade*`,
          ``,
          ...listLines,
          ``,
          `Reply with the plan number (e.g. *1* or *1 yearly*) to receive your instant UPI QR banner.`,
        ].join('\n'),
        engine: 'admin-vendor-command',
        keyIdx: -1,
        action: 'renew_plan_list',
      };
    }
  }

  // Natural-language LLM path with strictly last 10 RAM messages
  const sysPrompt = buildAdminVendorSystemPromptPreview(vendor, vm);
  const rawHistory = (ramStore.chats.get(phone) || []).slice(-10);
  const history = rawHistory.map((m) => ({
    role: m.role === 'assistant' ? 'assistant' : 'user',
    content: m.text,
  }));

  const keys = overrideApiKey ? [String(overrideApiKey).trim(), ...allAIKeys()] : allAIKeys();
  const groqKeys = keys.filter((k) => k.startsWith('gsk_'));
  const geminiKeys = keys.filter((k) => k.startsWith('AIza'));
  const provider = String(overrideProvider || ramStore.ai.provider || '').toLowerCase();
  const preferGemini = provider.includes('gemini') || provider.includes('google') || (geminiKeys.length > 0 && groqKeys.length === 0);

  if (preferGemini && geminiKeys.length > 0) {
    const gemRes = await callGeminiPool(geminiKeys, sysPrompt, history, ramStore.ai.temperature || 0.7);
    if (gemRes?.text) {
      return { reply: gemRes.text.trim(), engine: `admin-ai:gemini:${gemRes.model}`, keyIdx: gemRes.keyIdx };
    }
  }
  if (groqKeys.length > 0) {
    const groqRes = await callGroqPool(groqKeys, sysPrompt, history, ramStore.ai.temperature || 0.7);
    if (groqRes?.text) {
      return { reply: groqRes.text.trim(), engine: `admin-ai:groq:${groqRes.model}`, keyIdx: groqRes.keyIdx };
    }
  }
  if (!preferGemini && geminiKeys.length > 0) {
    const gemRes = await callGeminiPool(geminiKeys, sysPrompt, history, ramStore.ai.temperature || 0.7);
    if (gemRes?.text) {
      return { reply: gemRes.text.trim(), engine: `admin-ai:gemini:${gemRes.model}`, keyIdx: gemRes.keyIdx };
    }
  }

  return {
    reply: `👋 Hi! I'm the WaMart Vendor Support Manager. Reply *help* to view quick self-service commands (my plan, renew, store status, mall status).`,
    engine: 'admin-vendor-fallback',
    keyIdx: -1,
  };
}

app.post('/api/ai/test', async (req: Request, res: Response) => {
  const query = String(req.body?.query || '').trim();
  const phone = normalizePhone(req.body?.customer_phone || '');
  const mediaType = String(req.body?.media_type || 'text').toLowerCase();
  if (!query || !phone) {
    res.status(400).json({ status: 'error', message: 'query and customer_phone are required' });
    return;
  }

  // If customer sends an extra message before bot finishes replying, IGNORE for AI & return hardcoded peaceful quoted notice
  if (previewCustomerInFlight.has(phone)) {
    const alreadyNotified = previewSplitNoticeSent.has(phone);
    previewSplitNoticeSent.add(phone);
    if (!alreadyNotified) {
      pushLiveEvent(phone, SPLIT_MESSAGE_PEACEFUL_NOTICE, 'outgoing_ai');
    }
    res.json({
      status: 'success',
      ignored_split_message: true,
      quote_reply: true,
      reply: alreadyNotified ? '' : SPLIT_MESSAGE_PEACEFUL_NOTICE,
      ai_reply_text: alreadyNotified ? '' : SPLIT_MESSAGE_PEACEFUL_NOTICE,
      actions: [{ hand: 'split_message_ignored', payload: { quoted_reply: true } }],
      engine: 'split-message-guard',
      key_used_index: -1,
    });
    return;
  }

  previewCustomerInFlight.add(phone);
  previewSplitNoticeSent.delete(phone);

  try {
    // 1-second deliberate read delay before reading & processing the message
    await new Promise((resolve) => setTimeout(resolve, MESSAGE_READ_DELAY_MS));

    // Dual-mode branch: Admin Vendor Manager disables customer shopping/cart/ordering completely
    if (ramStore.botMode === 'admin_vendor_manager') {
      const displayIncoming = mediaType === 'voice_note' ? `🎙️ [Voice Note]: ${query}` : query;
      appendChatRAM(phone, 'customer', 'incoming', displayIncoming);
      pushLiveEvent(phone, displayIncoming, 'incoming');

      const adminResult = await processAdminVendorTextPreview(phone, query, req.body?.provider, req.body?.api_key);
      if (adminResult.reply) {
        appendChatRAM(phone, 'assistant', 'outgoing_ai', adminResult.reply);
        pushLiveEvent(phone, adminResult.reply, 'outgoing_ai');
      }
      res.json({
        status: 'success',
        bot_mode: ramStore.botMode,
        reply: adminResult.reply,
        ai_reply_text: adminResult.reply,
        engine: adminResult.engine,
        payment_qr_data_url: adminResult.qrDataUrl || undefined,
        actions: adminResult.action ? [{ hand: adminResult.action, payload: adminResult.payload || {} }] : [],
        key_used_index: adminResult.keyIdx,
      });
      return;
    }

    const { customer: cust, changed: custChanged } = upsertCustomerWithChangeCheckRAM({ wa_number: phone, source: 'whatsapp' });
    if (cust && custChanged) {
      void pushToCentralAsync('/api/vendor-bot/events/customer', { event: 'customer.upserted', source: 'whatsapp', customer: cust });
    }

    const isFirstTurn = !hasPriorAssistantReplyPreview(phone);

    const displayIncoming = mediaType === 'voice_note' ? `🎙️ [Voice Note]: ${query}` : query;

    appendChatRAM(phone, 'customer', 'incoming', displayIncoming);
    pushLiveEvent(phone, displayIncoming, 'incoming');

    const result = await runOctopusAI(phone, query, req.body?.provider, req.body?.api_key, mediaType);
    let finalReply = result.ai_reply_text;
    if (finalReply && isFirstTurn && !replyHasSingleMsgTip(finalReply)) {
      finalReply += FIRST_TURN_SINGLE_MESSAGE_TIP;
    }
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
      if (centralResp.qr_image_url) {
        (act.payload as any).qr_image_url = centralResp.qr_image_url;
      }
    } else if (act.hand === 'cancel_order') {
      const cust = ramStore.customers.get(phone);
      const cancelId = String(act.payload?.order_id || cust?.current_order_id || '').trim();
      if (cancelId) {
        stopPaymentVerificationPoller(cancelId);
      }
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
  } finally {
    previewCustomerInFlight.delete(phone);
    previewSplitNoticeSent.delete(phone);
  }
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

let startupRetryActive = false;
function startStartupBootstrapRetry(): void {
  if (startupRetryActive || ramStore.sync.bootstrapped || !centralNodeURL || !vendorApiKey) return;
  startupRetryActive = true;
  const delays = [5000, 10000, 15000, 20000, 30000, 45000, 60000];
  let attempt = 0;
  const scheduleNext = () => {
    if (ramStore.sync.bootstrapped || !centralNodeURL || !vendorApiKey) {
      startupRetryActive = false;
      return;
    }
    const waitMs = delays[Math.min(attempt, delays.length - 1)];
    attempt += 1;
    setTimeout(async () => {
      if (ramStore.sync.bootstrapped) {
        startupRetryActive = false;
        return;
      }
      const res = await bootstrapFromCentral();
      if (res.ok || ramStore.sync.bootstrapped) {
        startupRetryActive = false;
        return;
      }
      scheduleNext();
    }, waitMs);
  };
  scheduleNext();
}

// Boot Server + One-Time Startup Bootstrap (Zero Periodic Polling, Zero Recurring Heartbeat)
const server = app.listen(PORT, '0.0.0.0', () => {
  console.log(`🚀 Stateless Vendor WhatsApp Bot Node v2.3 running on http://0.0.0.0:${PORT}`);
  console.log(`🧠 Storage: 100% RAM-First | Chats: RAM-Only (Max 10) | Sync: Event-Driven (Zero Polling)`);
  void bootstrapFromCentral().then((res) => {
    if (!res.ok && centralNodeURL && vendorApiKey) {
      startStartupBootstrapRetry();
    }
  });
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
