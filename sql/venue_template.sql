-- =============================================
-- TEMPLATE CANONICO de BD por venue — VERIFICADO contra la BD viva de
-- 511 Events el 2026-07-21 (identico columna a columna en las 8 tablas
-- nucleo: events, orders, tickets, ticket_types, public_users,
-- verification_codes, organization_workers, roles).
--
-- Reglas:
--   1. Venues nuevos → ejecutar ESTE archivo entero.
--   2. La trazabilidad (approved_by, rejected_by...) vive en metadata
--      (jsonb), NO en columnas. No añadir columnas sin migrar la BD viva.
--   3. Si cambias este archivo, añade la migracion idempotente al final
--      Y aplicala a la BD viva de 511.
-- =============================================

-- =============================================
-- PULL API v2 - VENUE DATABASE TEMPLATE
-- =============================================
-- Este script crea todas las tablas necesarias para una nueva Venue DB
-- Ejecutar en cada nuevo proyecto Supabase de venue
-- =============================================

-- =============================================
-- ENUMS
-- =============================================

DO $$ BEGIN
    CREATE TYPE gender_type AS ENUM ('male', 'female', 'other');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE user_tier AS ENUM ('regular', 'vip', 'premium', 'blacklist');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE event_status AS ENUM ('draft', 'active', 'published', 'cancelled', 'completed');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE order_status AS ENUM ('pending', 'processing', 'confirmed', 'payment_authorized', 'payment_failed', 'checked_in', 'cancelled', 'refunded', 'expired');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE ticket_source AS ENUM ('order', 'guest_list', 'vip_list', 'group_reservation', 'comp', 'manual');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE reservation_status AS ENUM ('pending', 'confirmed', 'cancelled', 'completed', 'no_show');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE transaction_status AS ENUM ('pending', 'processing', 'completed', 'failed', 'refunded', 'partially_refunded');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE payment_gateway_type AS ENUM ('stripe', 'neonet', 'mercadopago', 'cash', 'transfer');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- =============================================
-- CORE TABLES
-- =============================================

-- Roles for organization workers
CREATE TABLE IF NOT EXISTS public.roles (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE,
    display_name text NOT NULL,
    description text,
    permissions jsonb DEFAULT '[]'::jsonb,
    hierarchy_level integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT roles_pkey PRIMARY KEY (id)
);

-- Organization workers (venue staff)
CREATE TABLE IF NOT EXISTS public.organization_workers (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE,
    first_name text NOT NULL,
    last_name text NOT NULL,
    phone text,
    dpi text,
    password_hash text NOT NULL,
    role_id uuid NOT NULL REFERENCES public.roles(id),
    is_active boolean DEFAULT true,
    profile_image text,
    failed_login_attempts integer DEFAULT 0,
    locked_until timestamp with time zone,
    last_login_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    deleted_at timestamp with time zone,
    CONSTRAINT organization_workers_pkey PRIMARY KEY (id)
);

-- Public users (customers)
CREATE TABLE IF NOT EXISTS public.public_users (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    email text UNIQUE,
    name text,
    surname text,
    phone text,
    phone_prefix text DEFAULT '+502',
    birth_date date,
    gender gender_type,
    profile_image text,
    tier user_tier DEFAULT 'regular',
    total_spent numeric DEFAULT 0,
    average_spend numeric DEFAULT 0,
    total_events_attended integer DEFAULT 0,
    tags jsonb DEFAULT '[]'::jsonb,
    preferences jsonb DEFAULT '{}'::jsonb,
    email_verified_at timestamp with time zone,
    phone_verified_at timestamp with time zone,
    last_login_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    deleted_at timestamp with time zone,
    CONSTRAINT public_users_pkey PRIMARY KEY (id)
);

-- Verification codes
CREATE TABLE IF NOT EXISTS public.verification_codes (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES public.public_users(id) ON DELETE CASCADE,
    code text NOT NULL,
    code_type text DEFAULT 'login' CHECK (code_type IN ('login', 'email_verify', 'phone_verify', 'password_reset')),
    expires_at timestamp with time zone NOT NULL,
    used boolean DEFAULT false,
    used_at timestamp with time zone,
    attempts integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT verification_codes_pkey PRIMARY KEY (id)
);

-- =============================================
-- EVENTS & TICKETS
-- =============================================

-- Events
CREATE TABLE IF NOT EXISTS public.events (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL,
    slug text UNIQUE,
    description text,
    short_description text,
    image text,
    cover_image text,
    gallery jsonb DEFAULT '[]'::jsonb,
    start_datetime timestamp with time zone NOT NULL,
    end_datetime timestamp with time zone,
    doors_open_time time without time zone,
    location text,
    address text,
    capacity integer,
    tickets_sold integer DEFAULT 0,
    status event_status DEFAULT 'draft',
    is_featured boolean DEFAULT false,
    is_private boolean DEFAULT false,
    min_age integer DEFAULT 18,
    dress_code text,
    music_genres jsonb DEFAULT '[]'::jsonb,
    artists jsonb DEFAULT '[]'::jsonb,
    use_vip_flow boolean DEFAULT false,
    use_guest_list boolean DEFAULT true,
    require_approval boolean DEFAULT false,
    settings jsonb DEFAULT '{}'::jsonb,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    deleted_at timestamp with time zone,
    created_by uuid REFERENCES public.organization_workers(id),
    CONSTRAINT events_pkey PRIMARY KEY (id)
);

-- Ticket types
CREATE TABLE IF NOT EXISTS public.ticket_types (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES public.events(id) ON DELETE CASCADE,
    name text NOT NULL,
    description text,
    price numeric NOT NULL CHECK (price >= 0),
    currency text DEFAULT 'GTQ',
    quantity_total integer NOT NULL CHECK (quantity_total >= 0),
    quantity_sold integer DEFAULT 0,
    quantity_reserved integer DEFAULT 0,
    max_per_order integer DEFAULT 10,
    min_per_order integer DEFAULT 1,
    sale_start timestamp with time zone,
    sale_end timestamp with time zone,
    is_active boolean DEFAULT true,
    is_visible boolean DEFAULT true,
    sort_order integer DEFAULT 0,
    benefits jsonb DEFAULT '[]'::jsonb,
    restrictions jsonb DEFAULT '{}'::jsonb,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT ticket_types_pkey PRIMARY KEY (id)
);

-- VIP ticket types (gender-based pricing)
CREATE TABLE IF NOT EXISTS public.event_vip_ticket_types (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES public.events(id) ON DELETE CASCADE,
    name text NOT NULL,
    description text,
    price_male numeric NOT NULL CHECK (price_male >= 0),
    price_female numeric NOT NULL CHECK (price_female >= 0),
    currency text DEFAULT 'GTQ',
    quantity_total integer NOT NULL,
    quantity_sold integer DEFAULT 0,
    includes_bottles boolean DEFAULT false,
    includes_table boolean DEFAULT false,
    max_guests integer DEFAULT 10,
    is_active boolean DEFAULT true,
    sort_order integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT event_vip_ticket_types_pkey PRIMARY KEY (id)
);

-- =============================================
-- ORDERS & TICKETS
-- =============================================

-- Orders
CREATE TABLE IF NOT EXISTS public.orders (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    order_number text NOT NULL UNIQUE,
    event_id uuid NOT NULL REFERENCES public.events(id),
    ticket_type_id uuid REFERENCES public.ticket_types(id),
    user_id uuid REFERENCES public.public_users(id),
    user_email text NOT NULL,
    user_name text,
    user_phone text,
    quantity integer NOT NULL CHECK (quantity > 0),
    unit_price numeric NOT NULL,
    subtotal numeric NOT NULL,
    discount_amount numeric DEFAULT 0,
    platform_fee numeric DEFAULT 0,
    total numeric NOT NULL,
    currency text DEFAULT 'GTQ',
    status order_status DEFAULT 'pending',
    payment_gateway payment_gateway_type,
    stripe_session_id text,
    stripe_payment_intent text,
    neonet_transaction_id text,
    mercadopago_preference_id text,
    paid_at timestamp with time zone,
    expires_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    cancellation_reason text,
    refunded_at timestamp with time zone,
    refund_amount numeric DEFAULT 0,
    metadata jsonb DEFAULT '{}'::jsonb,
    notes text,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT orders_pkey PRIMARY KEY (id)
);

-- Tickets
CREATE TABLE IF NOT EXISTS public.tickets (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    order_id uuid REFERENCES public.orders(id),
    event_id uuid NOT NULL REFERENCES public.events(id),
    ticket_type_id uuid REFERENCES public.ticket_types(id),
    ticket_type_name text,
    holder_id uuid REFERENCES public.public_users(id),
    owner_name text,
    owner_last_name text,
    owner_email text,
    owner_phone text,
    owner_gender gender_type,
    owner_dpi text,
    qr_token text NOT NULL UNIQUE,
    source ticket_source DEFAULT 'order',
    source_id uuid,
    price_paid numeric DEFAULT 0,
    currency text DEFAULT 'GTQ',
    checked_in_at timestamp with time zone,
    checked_in_by uuid REFERENCES public.organization_workers(id),
    is_valid boolean DEFAULT true,
    is_transferred boolean DEFAULT false,
    transferred_from uuid,
    transferred_at timestamp with time zone,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT tickets_pkey PRIMARY KEY (id)
);

-- =============================================
-- GROUP RESERVATIONS (Mesas/Botellas)
-- =============================================

-- Bottles catalog
CREATE TABLE IF NOT EXISTS public.vip_bottles (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL,
    brand text,
    category text NOT NULL,
    size_ml integer DEFAULT 750,
    price numeric NOT NULL CHECK (price >= 0),
    currency text DEFAULT 'GTQ',
    image text,
    description text,
    is_active boolean DEFAULT true,
    sort_order integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT vip_bottles_pkey PRIMARY KEY (id)
);

-- Mixers catalog
CREATE TABLE IF NOT EXISTS public.vip_mixers (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    name text NOT NULL,
    price numeric NOT NULL CHECK (price >= 0),
    currency text DEFAULT 'GTQ',
    is_active boolean DEFAULT true,
    sort_order integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT vip_mixers_pkey PRIMARY KEY (id)
);

-- Group reservations
CREATE TABLE IF NOT EXISTS public.group_reservations (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_number text NOT NULL UNIQUE,
    event_id uuid NOT NULL REFERENCES public.events(id),
    organizer_id uuid REFERENCES public.public_users(id),
    organizer_name text NOT NULL,
    organizer_email text NOT NULL,
    organizer_phone text,
    ticket_type_id uuid REFERENCES public.event_vip_ticket_types(id),
    table_number text,
    max_guests integer NOT NULL DEFAULT 10,
    confirmed_guests integer DEFAULT 0,
    status reservation_status DEFAULT 'pending',
    subtotal numeric DEFAULT 0,
    discount_amount numeric DEFAULT 0,
    platform_fee numeric DEFAULT 0,
    total numeric DEFAULT 0,
    currency text DEFAULT 'GTQ',
    deposit_amount numeric DEFAULT 0,
    deposit_paid boolean DEFAULT false,
    deposit_paid_at timestamp with time zone,
    payment_gateway payment_gateway_type,
    stripe_session_id text,
    paid_at timestamp with time zone,
    notes text,
    internal_notes text,
    approved_by uuid REFERENCES public.organization_workers(id),
    approved_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    cancellation_reason text,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT group_reservations_pkey PRIMARY KEY (id)
);

-- Group reservation guests
CREATE TABLE IF NOT EXISTS public.group_reservation_guests (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid NOT NULL REFERENCES public.group_reservations(id) ON DELETE CASCADE,
    user_id uuid REFERENCES public.public_users(id),
    name text NOT NULL,
    last_name text,
    email text,
    phone text,
    gender gender_type,
    dpi text,
    is_organizer boolean DEFAULT false,
    ticket_price numeric DEFAULT 0,
    has_paid boolean DEFAULT false,
    paid_at timestamp with time zone,
    payment_method text,
    stripe_session_id text,
    qr_token text UNIQUE,
    checked_in_at timestamp with time zone,
    checked_in_by uuid REFERENCES public.organization_workers(id),
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT group_reservation_guests_pkey PRIMARY KEY (id)
);

-- Group reservation bottles
CREATE TABLE IF NOT EXISTS public.group_reservation_bottles (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid NOT NULL REFERENCES public.group_reservations(id) ON DELETE CASCADE,
    bottle_id uuid NOT NULL REFERENCES public.vip_bottles(id),
    quantity integer NOT NULL DEFAULT 1 CHECK (quantity > 0),
    unit_price numeric NOT NULL,
    total_price numeric NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT group_reservation_bottles_pkey PRIMARY KEY (id)
);

-- Group reservation mixers
CREATE TABLE IF NOT EXISTS public.group_reservation_mixers (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid NOT NULL REFERENCES public.group_reservations(id) ON DELETE CASCADE,
    mixer_id uuid NOT NULL REFERENCES public.vip_mixers(id),
    quantity integer NOT NULL DEFAULT 1 CHECK (quantity > 0),
    unit_price numeric NOT NULL,
    total_price numeric NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT group_reservation_mixers_pkey PRIMARY KEY (id)
);

-- =============================================
-- VIP LIST RESERVATIONS
-- =============================================

-- VIP list reservations
CREATE TABLE IF NOT EXISTS public.vip_list_reservations (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_number text NOT NULL UNIQUE,
    event_id uuid NOT NULL REFERENCES public.events(id),
    organizer_id uuid REFERENCES public.public_users(id),
    organizer_name text NOT NULL,
    organizer_email text NOT NULL,
    organizer_phone text,
    ticket_type_id uuid REFERENCES public.event_vip_ticket_types(id),
    max_guests integer NOT NULL DEFAULT 10,
    confirmed_guests integer DEFAULT 0,
    status reservation_status DEFAULT 'pending',
    total numeric DEFAULT 0,
    currency text DEFAULT 'GTQ',
    payment_gateway payment_gateway_type,
    paid_at timestamp with time zone,
    notes text,
    approved_by uuid REFERENCES public.organization_workers(id),
    approved_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT vip_list_reservations_pkey PRIMARY KEY (id)
);

-- VIP list guests
CREATE TABLE IF NOT EXISTS public.vip_list_guests (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid NOT NULL REFERENCES public.vip_list_reservations(id) ON DELETE CASCADE,
    user_id uuid REFERENCES public.public_users(id),
    name text NOT NULL,
    last_name text,
    email text,
    phone text,
    gender gender_type,
    is_organizer boolean DEFAULT false,
    ticket_price numeric DEFAULT 0,
    has_paid boolean DEFAULT false,
    paid_at timestamp with time zone,
    qr_token text UNIQUE,
    checked_in_at timestamp with time zone,
    checked_in_by uuid REFERENCES public.organization_workers(id),
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT vip_list_guests_pkey PRIMARY KEY (id)
);

-- VIP list bottles
CREATE TABLE IF NOT EXISTS public.vip_list_bottles (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid NOT NULL REFERENCES public.vip_list_reservations(id) ON DELETE CASCADE,
    bottle_id uuid NOT NULL REFERENCES public.vip_bottles(id),
    quantity integer NOT NULL DEFAULT 1,
    unit_price numeric NOT NULL,
    total_price numeric NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT vip_list_bottles_pkey PRIMARY KEY (id)
);

-- =============================================
-- GUEST LIST
-- =============================================

-- Guest list types
CREATE TABLE IF NOT EXISTS public.guest_list_types (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES public.events(id) ON DELETE CASCADE,
    name text NOT NULL,
    description text,
    max_signups integer,
    current_signups integer DEFAULT 0,
    benefits text,
    is_active boolean DEFAULT true,
    signup_start timestamp with time zone,
    signup_end timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT guest_list_types_pkey PRIMARY KEY (id)
);

-- Guest list signups
CREATE TABLE IF NOT EXISTS public.guest_list_signups (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    guest_list_type_id uuid NOT NULL REFERENCES public.guest_list_types(id) ON DELETE CASCADE,
    event_id uuid NOT NULL REFERENCES public.events(id),
    user_id uuid REFERENCES public.public_users(id),
    name text NOT NULL,
    last_name text,
    email text,
    phone text,
    gender gender_type,
    plus_one_name text,
    plus_one_gender gender_type,
    status text DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'checked_in')),
    qr_token text UNIQUE,
    approved_by uuid REFERENCES public.organization_workers(id),
    approved_at timestamp with time zone,
    checked_in_at timestamp with time zone,
    checked_in_by uuid REFERENCES public.organization_workers(id),
    notes text,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT guest_list_signups_pkey PRIMARY KEY (id)
);

-- =============================================
-- TRANSACTIONS (Venue-level)
-- =============================================

-- Transactions
CREATE TABLE IF NOT EXISTS public.transactions (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    transaction_number text NOT NULL UNIQUE,
    transaction_type text NOT NULL CHECK (transaction_type IN ('ticket_sale', 'group_reservation', 'vip_list', 'guest_payment', 'refund')),
    status transaction_status DEFAULT 'pending',
    gross_amount numeric NOT NULL,
    platform_fee_percent numeric DEFAULT 11.20,
    platform_fee_amount numeric NOT NULL,
    gateway_fee_amount numeric DEFAULT 0,
    net_amount numeric NOT NULL,
    currency text DEFAULT 'GTQ',
    event_id uuid REFERENCES public.events(id),
    user_id uuid REFERENCES public.public_users(id),
    order_id uuid REFERENCES public.orders(id),
    group_reservation_id uuid REFERENCES public.group_reservations(id),
    vip_list_reservation_id uuid REFERENCES public.vip_list_reservations(id),
    payment_gateway payment_gateway_type,
    stripe_payment_intent text,
    stripe_charge_id text,
    neonet_transaction_id text,
    neonet_authorization_code text,
    mercadopago_payment_id text,
    payer_name text,
    payer_email text,
    card_last4 text,
    card_brand text,
    metadata jsonb DEFAULT '{}'::jsonb,
    refund_reason text,
    refunded_amount numeric DEFAULT 0,
    captured_at timestamp with time zone,
    refunded_at timestamp with time zone,
    failed_at timestamp with time zone,
    failure_reason text,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT transactions_pkey PRIMARY KEY (id)
);

-- Transaction line items
CREATE TABLE IF NOT EXISTS public.transaction_line_items (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    transaction_id uuid NOT NULL REFERENCES public.transactions(id) ON DELETE CASCADE,
    item_type text NOT NULL CHECK (item_type IN ('ticket', 'bottle', 'mixer', 'table', 'service_fee', 'platform_fee', 'discount')),
    item_id uuid,
    item_name text NOT NULL,
    quantity integer NOT NULL DEFAULT 1,
    unit_price numeric NOT NULL,
    total_price numeric NOT NULL,
    gender gender_type,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT transaction_line_items_pkey PRIMARY KEY (id)
);

-- =============================================
-- NOTIFICATIONS
-- =============================================

CREATE TABLE IF NOT EXISTS public.notifications (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    recipient_id uuid NOT NULL,
    recipient_type text NOT NULL DEFAULT 'staff' CHECK (recipient_type IN ('staff', 'user')),
    type text NOT NULL,
    title text NOT NULL,
    message text,
    data jsonb DEFAULT '{}'::jsonb,
    read_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT notifications_pkey PRIMARY KEY (id)
);

-- =============================================
-- USER SESSIONS & TOKENS
-- =============================================

CREATE TABLE IF NOT EXISTS public.user_sessions (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES public.public_users(id) ON DELETE CASCADE,
    token_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    ip_address inet,
    user_agent text,
    device_type text,
    is_revoked boolean DEFAULT false,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT user_sessions_pkey PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS public.user_refresh_tokens (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES public.public_users(id) ON DELETE CASCADE,
    token_hash text NOT NULL,
    family_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    is_revoked boolean DEFAULT false,
    ip_address inet,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT user_refresh_tokens_pkey PRIMARY KEY (id)
);

-- =============================================
-- REVENUE SUMMARIES
-- =============================================

CREATE TABLE IF NOT EXISTS public.daily_revenue_summary (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    date date NOT NULL UNIQUE,
    total_gross numeric DEFAULT 0,
    total_platform_fee numeric DEFAULT 0,
    total_net numeric DEFAULT 0,
    ticket_sales_count integer DEFAULT 0,
    ticket_sales_gross numeric DEFAULT 0,
    group_reservations_count integer DEFAULT 0,
    group_reservations_gross numeric DEFAULT 0,
    vip_list_count integer DEFAULT 0,
    vip_list_gross numeric DEFAULT 0,
    refunds_count integer DEFAULT 0,
    refunds_amount numeric DEFAULT 0,
    unique_customers integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT daily_revenue_summary_pkey PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS public.event_revenue_summary (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL UNIQUE REFERENCES public.events(id),
    total_gross numeric DEFAULT 0,
    total_platform_fee numeric DEFAULT 0,
    total_net numeric DEFAULT 0,
    tickets_sold integer DEFAULT 0,
    tickets_revenue numeric DEFAULT 0,
    group_reservations_count integer DEFAULT 0,
    group_reservations_revenue numeric DEFAULT 0,
    vip_list_count integer DEFAULT 0,
    vip_list_revenue numeric DEFAULT 0,
    bottles_sold integer DEFAULT 0,
    bottles_revenue numeric DEFAULT 0,
    guests_checked_in integer DEFAULT 0,
    unique_customers integer DEFAULT 0,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT event_revenue_summary_pkey PRIMARY KEY (id)
);

-- =============================================
-- AUDIT LOG
-- =============================================

CREATE TABLE IF NOT EXISTS public.audit_log (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    actor_type text NOT NULL CHECK (actor_type IN ('user', 'staff', 'system', 'webhook')),
    actor_id uuid,
    actor_email text,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid,
    old_values jsonb,
    new_values jsonb,
    ip_address inet,
    user_agent text,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT audit_log_pkey PRIMARY KEY (id)
);

-- =============================================
-- INDEXES
-- =============================================

-- Events
CREATE INDEX IF NOT EXISTS idx_events_status ON public.events(status);
CREATE INDEX IF NOT EXISTS idx_events_start ON public.events(start_datetime);
CREATE INDEX IF NOT EXISTS idx_events_slug ON public.events(slug);

-- Ticket types
CREATE INDEX IF NOT EXISTS idx_ticket_types_event ON public.ticket_types(event_id);

-- Orders
CREATE INDEX IF NOT EXISTS idx_orders_event ON public.orders(event_id);
CREATE INDEX IF NOT EXISTS idx_orders_user ON public.orders(user_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON public.orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_number ON public.orders(order_number);

-- Tickets
CREATE INDEX IF NOT EXISTS idx_tickets_event ON public.tickets(event_id);
CREATE INDEX IF NOT EXISTS idx_tickets_order ON public.tickets(order_id);
CREATE INDEX IF NOT EXISTS idx_tickets_holder ON public.tickets(holder_id);
CREATE INDEX IF NOT EXISTS idx_tickets_qr ON public.tickets(qr_token);
CREATE INDEX IF NOT EXISTS idx_tickets_checkin ON public.tickets(event_id, checked_in_at);

-- Group reservations
CREATE INDEX IF NOT EXISTS idx_group_reservations_event ON public.group_reservations(event_id);
CREATE INDEX IF NOT EXISTS idx_group_reservations_status ON public.group_reservations(status);

-- Transactions
CREATE INDEX IF NOT EXISTS idx_transactions_event ON public.transactions(event_id);
CREATE INDEX IF NOT EXISTS idx_transactions_user ON public.transactions(user_id);
CREATE INDEX IF NOT EXISTS idx_transactions_status ON public.transactions(status);
CREATE INDEX IF NOT EXISTS idx_transactions_created ON public.transactions(created_at DESC);

-- Users
CREATE INDEX IF NOT EXISTS idx_public_users_email ON public.public_users(email);
CREATE INDEX IF NOT EXISTS idx_organization_workers_email ON public.organization_workers(email);

-- Notifications
CREATE INDEX IF NOT EXISTS idx_notifications_recipient ON public.notifications(recipient_id, recipient_type);
CREATE INDEX IF NOT EXISTS idx_notifications_unread ON public.notifications(recipient_id) WHERE read_at IS NULL;

-- =============================================
-- FUNCTIONS
-- =============================================

-- Generate order number
CREATE OR REPLACE FUNCTION generate_order_number()
RETURNS text AS $$
BEGIN
    RETURN 'ORD-' || to_char(now(), 'YYYYMMDD') || '-' || upper(substr(md5(random()::text), 1, 6));
END;
$$ LANGUAGE plpgsql;

-- Generate reservation number
CREATE OR REPLACE FUNCTION generate_reservation_number()
RETURNS text AS $$
BEGIN
    RETURN 'RES-' || to_char(now(), 'YYYYMMDD') || '-' || upper(substr(md5(random()::text), 1, 6));
END;
$$ LANGUAGE plpgsql;

-- Generate transaction number
CREATE OR REPLACE FUNCTION generate_transaction_number()
RETURNS text AS $$
BEGIN
    RETURN 'TXN-' || to_char(now(), 'YYYYMMDD') || '-' || upper(substr(md5(random()::text), 1, 8));
END;
$$ LANGUAGE plpgsql;

-- Generate QR token
CREATE OR REPLACE FUNCTION generate_qr_token()
RETURNS text AS $$
BEGIN
    RETURN encode(gen_random_bytes(24), 'hex');
END;
$$ LANGUAGE plpgsql;

-- =============================================
-- TRIGGERS
-- =============================================

-- Auto-generate order number
CREATE OR REPLACE FUNCTION set_order_number()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.order_number IS NULL OR NEW.order_number = '' THEN
        NEW.order_number := generate_order_number();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_set_order_number ON public.orders;
CREATE TRIGGER trigger_set_order_number
    BEFORE INSERT ON public.orders
    FOR EACH ROW EXECUTE FUNCTION set_order_number();

-- Auto-generate reservation number (group)
CREATE OR REPLACE FUNCTION set_group_reservation_number()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.reservation_number IS NULL OR NEW.reservation_number = '' THEN
        NEW.reservation_number := generate_reservation_number();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_set_group_reservation_number ON public.group_reservations;
CREATE TRIGGER trigger_set_group_reservation_number
    BEFORE INSERT ON public.group_reservations
    FOR EACH ROW EXECUTE FUNCTION set_group_reservation_number();

-- Auto-generate reservation number (vip list)
DROP TRIGGER IF EXISTS trigger_set_vip_list_reservation_number ON public.vip_list_reservations;
CREATE TRIGGER trigger_set_vip_list_reservation_number
    BEFORE INSERT ON public.vip_list_reservations
    FOR EACH ROW EXECUTE FUNCTION set_group_reservation_number();

-- Auto-generate transaction number
CREATE OR REPLACE FUNCTION set_transaction_number()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.transaction_number IS NULL OR NEW.transaction_number = '' THEN
        NEW.transaction_number := generate_transaction_number();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_set_transaction_number ON public.transactions;
CREATE TRIGGER trigger_set_transaction_number
    BEFORE INSERT ON public.transactions
    FOR EACH ROW EXECUTE FUNCTION set_transaction_number();

-- Auto-generate QR token for tickets
CREATE OR REPLACE FUNCTION set_ticket_qr_token()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.qr_token IS NULL OR NEW.qr_token = '' THEN
        NEW.qr_token := generate_qr_token();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_set_ticket_qr_token ON public.tickets;
CREATE TRIGGER trigger_set_ticket_qr_token
    BEFORE INSERT ON public.tickets
    FOR EACH ROW EXECUTE FUNCTION set_ticket_qr_token();

-- Updated_at trigger function
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Apply updated_at trigger to relevant tables
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['events', 'ticket_types', 'orders', 'tickets', 'group_reservations',
                              'vip_list_reservations', 'public_users', 'organization_workers',
                              'transactions', 'daily_revenue_summary', 'event_revenue_summary']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trigger_updated_at ON public.%I', t);
        EXECUTE format('CREATE TRIGGER trigger_updated_at BEFORE UPDATE ON public.%I
                        FOR EACH ROW EXECUTE FUNCTION update_updated_at()', t);
    END LOOP;
END;
$$;

-- =============================================
-- ROW LEVEL SECURITY
-- =============================================

ALTER TABLE public.roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.organization_workers ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.public_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.verification_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.events ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.ticket_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.event_vip_ticket_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.vip_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.vip_mixers ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_reservation_guests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_reservation_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_reservation_mixers ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.vip_list_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.vip_list_guests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.vip_list_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.guest_list_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.guest_list_signups ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.transaction_line_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.daily_revenue_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.event_revenue_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.audit_log ENABLE ROW LEVEL SECURITY;

-- Service role policies (full access for API)
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['roles', 'organization_workers', 'public_users', 'verification_codes',
                              'events', 'ticket_types', 'event_vip_ticket_types', 'orders', 'tickets',
                              'vip_bottles', 'vip_mixers', 'group_reservations', 'group_reservation_guests',
                              'group_reservation_bottles', 'group_reservation_mixers', 'vip_list_reservations',
                              'vip_list_guests', 'vip_list_bottles', 'guest_list_types', 'guest_list_signups',
                              'transactions', 'transaction_line_items', 'notifications', 'user_sessions',
                              'user_refresh_tokens', 'daily_revenue_summary', 'event_revenue_summary', 'audit_log']
    LOOP
        EXECUTE format('CREATE POLICY "Service role full access" ON public.%I FOR ALL USING (true)', t);
    END LOOP;
END;
$$;

-- =============================================
-- SEED DATA: Default Roles
-- =============================================

INSERT INTO public.roles (name, display_name, description, permissions, hierarchy_level) VALUES
    ('admin', 'Administrador', 'Acceso completo al sistema', '["all"]', 100),
    ('manager', 'Gerente', 'Gestión de eventos y personal', '["events", "staff", "reports", "orders"]', 80),
    ('supervisor', 'Supervisor', 'Supervisión de operaciones', '["events", "orders", "checkin"]', 60),
    ('doorman', 'Portero', 'Control de acceso y check-in', '["checkin", "guest_list"]', 40),
    ('promoter', 'Promotor', 'Gestión de guest lists', '["guest_list", "reservations"]', 30),
    ('staff', 'Staff', 'Personal general', '["checkin"]', 20)
ON CONFLICT (name) DO NOTHING;

-- =============================================
-- VERIFICATION
-- =============================================

DO $$
DECLARE
    table_count integer;
BEGIN
    SELECT COUNT(*) INTO table_count
    FROM information_schema.tables
    WHERE table_schema = 'public' AND table_type = 'BASE TABLE';

    RAISE NOTICE '✅ Venue database template ejecutado correctamente';
    RAISE NOTICE '📊 Total de tablas creadas: %', table_count;
END;
$$;


-- =============================================
-- MIGRACIONES POST-LANZAMIENTO (idempotentes — seguras sobre BDs existentes)
-- =============================================

-- 2026-07: estados de pago del flujo publico/privado Cybersource
ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'payment_authorized';
ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'payment_failed';
ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'checked_in';

-- 2026-07: push notifications del staff (FCM v1). La tenancy es la BD:
-- no lleva venue_id.
CREATE TABLE IF NOT EXISTS public.staff_push_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    employee_id UUID REFERENCES organization_workers(id) ON DELETE CASCADE,
    push_token TEXT NOT NULL UNIQUE,
    device_type TEXT,
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_staff_push_tokens_active
    ON staff_push_tokens(is_active) WHERE is_active;
