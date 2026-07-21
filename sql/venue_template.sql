-- =============================================
-- TEMPLATE CANONICO de BD por venue (2026-07-21)
-- venue_database_template.sql queda DEPRECADO (era el esquema viejo con el
-- que se creo la BD viva de 511 — por eso la BD viva NO tiene approved_by/
-- rejected_by en orders ni rejected_at en guest_list_signups; el codigo
-- escribe esa trazabilidad en metadata y debe seguir haciendolo).
-- =============================================

-- =============================================
-- PULL - VENUE DATABASE TEMPLATE
-- =============================================
-- Este schema se usa para crear una nueva base de datos por cada venue
-- Cada venue tiene su propio Supabase project con estas tablas
--
-- Para crear un nuevo venue:
-- 1. Crear proyecto en Supabase
-- 2. Ejecutar este SQL en el nuevo proyecto
-- 3. Registrar las credenciales en Central DB (venue_database_configs)
-- =============================================

-- Enable extensions
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- =============================================
-- ENUMS
-- =============================================

DO $$ BEGIN
    CREATE TYPE order_status AS ENUM (
        'pending', 'processing', 'confirmed', 'payment_authorized',
        'payment_failed', 'checked_in', 'cancelled', 'refunded', 'expired'
    );
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE transaction_type AS ENUM (
        'individual_ticket', 'group_organizer', 'group_guest',
        'vip_list', 'refund', 'partial_refund', 'adjustment'
    );
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE transaction_status AS ENUM (
        'pending', 'captured', 'refunded', 'partially_refunded',
        'failed', 'cancelled', 'expired'
    );
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE payment_gateway_type AS ENUM ('stripe', 'neonet', 'mercadopago', 'paypal');
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE gender_type AS ENUM ('male', 'female', 'other', 'prefer_not_to_say');
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE reservation_status AS ENUM (
        'pending', 'approved', 'rejected', 'cancelled',
        'payment_pending', 'completed', 'expired'
    );
EXCEPTION WHEN duplicate_object THEN null; END $$;

DO $$ BEGIN
    CREATE TYPE ticket_source AS ENUM (
        'order', 'group_reservation', 'vip_list', 'guest_list', 'manual'
    );
EXCEPTION WHEN duplicate_object THEN null; END $$;

-- =============================================
-- PUBLIC USERS (Customers)
-- =============================================

CREATE TABLE IF NOT EXISTS public_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Basic Info
    name TEXT,
    surname TEXT,
    email TEXT UNIQUE,
    phone TEXT,
    phone_prefix TEXT DEFAULT '+502',

    -- Profile
    birth_date DATE,
    gender gender_type,
    profile_image TEXT,

    -- Stats
    tier TEXT DEFAULT 'regular' CHECK (tier IN ('regular', 'vip')),
    total_spent DECIMAL(12, 2) DEFAULT 0,
    average_spend DECIMAL(10, 2) DEFAULT 0,
    total_events_attended INT DEFAULT 0,

    -- Tags for segmentation
    tags JSONB DEFAULT '[]',

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_public_users_email ON public_users(email);
CREATE INDEX IF NOT EXISTS idx_public_users_phone ON public_users(phone);

-- =============================================
-- ROLES (Staff roles)
-- =============================================

CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    permissions JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Insert default roles
INSERT INTO roles (name, permissions) VALUES
    ('admin', '["all"]'),
    ('manager', '["events", "orders", "staff", "analytics"]'),
    ('staff', '["events", "orders", "validate"]'),
    ('validator', '["validate"]'),
    ('cashier', '["orders"]')
ON CONFLICT (name) DO NOTHING;

-- =============================================
-- ORGANIZATION WORKERS (Venue Staff)
-- =============================================

CREATE TABLE IF NOT EXISTS organization_workers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Auth
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    password_changed_at TIMESTAMPTZ,

    -- Profile
    first_name TEXT NOT NULL,
    last_name TEXT NOT NULL,
    dpi_hash TEXT,  -- National ID hash for verification
    phone TEXT,
    avatar_url TEXT,

    -- Role
    role_id UUID NOT NULL REFERENCES roles(id),

    -- Status
    is_active BOOLEAN DEFAULT true,
    last_login_at TIMESTAMPTZ,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_org_workers_email ON organization_workers(email);
CREATE INDEX IF NOT EXISTS idx_org_workers_active ON organization_workers(is_active) WHERE is_active = true;

-- =============================================
-- EVENTS
-- =============================================

CREATE TABLE IF NOT EXISTS events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Basic Info
    name TEXT NOT NULL,
    slug TEXT UNIQUE,
    description TEXT,
    image TEXT,

    -- Date & Time
    event_date DATE NOT NULL,
    start_time TIME NOT NULL,
    end_time TIME NOT NULL,

    -- Location (can override venue location)
    custom_location TEXT,

    -- Capacity
    ticket_limit INT,
    table_capacity INT DEFAULT 0,

    -- Requirements
    min_age INT DEFAULT 18,
    dress_code TEXT,
    requirements JSONB,

    -- Status
    is_active BOOLEAN DEFAULT true,
    is_published BOOLEAN DEFAULT false,
    published_at TIMESTAMPTZ,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_events_date ON events(event_date);
CREATE INDEX IF NOT EXISTS idx_events_slug ON events(slug);
CREATE INDEX IF NOT EXISTS idx_events_active ON events(is_active, is_published) WHERE is_active = true AND is_published = true;

-- =============================================
-- TICKET TYPES
-- =============================================

CREATE TABLE IF NOT EXISTS ticket_types (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,

    -- Basic Info
    name TEXT NOT NULL,
    description TEXT,
    benefits TEXT,

    -- Pricing
    price DECIMAL(10, 2) NOT NULL,
    base_price DECIMAL(10, 2),

    -- Gender pricing
    has_gender_pricing BOOLEAN DEFAULT false,
    male_price DECIMAL(10, 2),
    female_price DECIMAL(10, 2),

    -- Quantity
    initial_quantity INT NOT NULL,
    available_quantity INT NOT NULL,

    -- Limits per order
    min_quantity INT DEFAULT 1 CHECK (min_quantity >= 1),
    max_quantity INT DEFAULT 10 CHECK (max_quantity >= 1),

    -- Group ticket
    is_group BOOLEAN DEFAULT false,

    -- Display
    sort_order INT DEFAULT 0,
    is_active BOOLEAN DEFAULT true,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ticket_types_event ON ticket_types(event_id);

-- =============================================
-- EVENT VIP TICKET TYPES (With gender pricing for VIP)
-- =============================================

CREATE TABLE IF NOT EXISTS event_vip_ticket_types (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,

    name TEXT NOT NULL,
    male_price DECIMAL(10, 2) NOT NULL DEFAULT 0,
    female_price DECIMAL(10, 2) NOT NULL DEFAULT 0,

    min_quantity INT DEFAULT 1 CHECK (min_quantity >= 1),
    max_quantity INT DEFAULT 20,

    sort_order INT DEFAULT 0,
    is_active BOOLEAN DEFAULT true,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_event_vip_types_event ON event_vip_ticket_types(event_id);

-- =============================================
-- ORDERS (Individual ticket purchases)
-- =============================================

CREATE TABLE IF NOT EXISTS orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_number TEXT UNIQUE,

    -- References
    event_id UUID NOT NULL REFERENCES events(id),
    ticket_type_id UUID NOT NULL REFERENCES ticket_types(id),
    user_id UUID NOT NULL REFERENCES public_users(id),

    -- Order Details
    quantity INT NOT NULL,
    total DECIMAL(10, 2) NOT NULL,
    currency TEXT DEFAULT 'GTQ',

    -- Customer Info (snapshot at order time)
    user_name TEXT,
    user_email TEXT,

    -- Status
    status order_status DEFAULT 'pending',

    -- Payment
    payment_gateway payment_gateway_type,
    stripe_session_id TEXT,
    stripe_payment_intent TEXT,
    neonet_transaction_id TEXT,
    neonet_authorization_code TEXT,

    -- Payment Link (for async payment)
    payment_link_code TEXT UNIQUE,
    expires_at TIMESTAMPTZ,

    -- Approval (for manual approval flow)
    approved_by UUID REFERENCES organization_workers(id),
    approved_at TIMESTAMPTZ,
    rejected_by UUID REFERENCES organization_workers(id),
    rejected_at TIMESTAMPTZ,
    reject_reason TEXT,

    -- Tickets data (snapshot)
    tickets_data JSONB,
    metadata JSONB,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    paid_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_orders_event ON orders(event_id);
CREATE INDEX IF NOT EXISTS idx_orders_user ON orders(user_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_number ON orders(order_number);
CREATE INDEX IF NOT EXISTS idx_orders_payment_link ON orders(payment_link_code);

-- =============================================
-- TICKETS (Generated entries)
-- =============================================

CREATE TABLE IF NOT EXISTS tickets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- References
    order_id UUID REFERENCES orders(id),
    event_id UUID NOT NULL REFERENCES events(id),
    ticket_type_id UUID REFERENCES ticket_types(id),
    holder_id UUID NOT NULL REFERENCES public_users(id),
    group_reservation_id UUID,  -- FK added later

    -- QR Code
    qr_token TEXT NOT NULL UNIQUE,

    -- Source
    source ticket_source DEFAULT 'order',
    ticket_type_name TEXT,

    -- Holder Info (snapshot)
    owner_name TEXT,
    owner_last_name TEXT,
    owner_email TEXT,
    owner_phone TEXT,
    owner_phone_prefix TEXT DEFAULT '+502',
    owner_gender gender_type,
    owner_birthdate DATE,

    -- Check-in
    checked_in_at TIMESTAMPTZ,
    checked_in_by UUID REFERENCES organization_workers(id),
    validated_at TIMESTAMPTZ,

    -- PDF
    pdf_url TEXT,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tickets_event ON tickets(event_id);
CREATE INDEX IF NOT EXISTS idx_tickets_holder ON tickets(holder_id);
CREATE INDEX IF NOT EXISTS idx_tickets_qr ON tickets(qr_token);
CREATE INDEX IF NOT EXISTS idx_tickets_order ON tickets(order_id);
CREATE INDEX IF NOT EXISTS idx_tickets_checkin ON tickets(checked_in_at) WHERE checked_in_at IS NOT NULL;

-- =============================================
-- GROUP RESERVATION STATUS
-- =============================================

CREATE TABLE IF NOT EXISTS group_reservation_status (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

INSERT INTO group_reservation_status (name) VALUES
    ('pending'),
    ('approved'),
    ('rejected'),
    ('payment_pending'),
    ('completed'),
    ('cancelled'),
    ('expired')
ON CONFLICT (name) DO NOTHING;

-- =============================================
-- GROUP RESERVATIONS
-- =============================================

CREATE TABLE IF NOT EXISTS group_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- References
    event_id UUID NOT NULL REFERENCES events(id),
    organizer_id UUID NOT NULL REFERENCES public_users(id),
    status_id INT NOT NULL DEFAULT 1 REFERENCES group_reservation_status(id),

    -- Details
    guest_count INT NOT NULL,
    total_amount DECIMAL(10, 2) NOT NULL,
    paid_amount DECIMAL(10, 2) DEFAULT 0,

    -- Codes
    management_code TEXT NOT NULL UNIQUE,
    payment_link_code TEXT NOT NULL UNIQUE,
    host_paid_access_code TEXT UNIQUE,

    -- Bottles
    bottle_voucher_token TEXT UNIQUE,
    bottles_redeemed_at TIMESTAMPTZ,
    bottles_redeemed_by UUID REFERENCES organization_workers(id),

    -- Deadlines
    deadline_at TIMESTAMPTZ,

    -- Metadata
    metadata JSONB,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    approved_at TIMESTAMPTZ,
    rejected_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_group_res_event ON group_reservations(event_id);
CREATE INDEX IF NOT EXISTS idx_group_res_organizer ON group_reservations(organizer_id);
CREATE INDEX IF NOT EXISTS idx_group_res_management ON group_reservations(management_code);
CREATE INDEX IF NOT EXISTS idx_group_res_payment_link ON group_reservations(payment_link_code);

-- Add FK to tickets
ALTER TABLE tickets ADD CONSTRAINT fk_tickets_group_reservation
    FOREIGN KEY (group_reservation_id) REFERENCES group_reservations(id);

-- =============================================
-- GROUP RESERVATION GUESTS
-- =============================================

CREATE TABLE IF NOT EXISTS group_reservation_guests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL REFERENCES group_reservations(id) ON DELETE CASCADE,
    user_id UUID REFERENCES public_users(id),
    ticket_id UUID REFERENCES tickets(id),

    -- Guest Info
    name TEXT NOT NULL,
    last_name TEXT,
    email TEXT,
    gender gender_type,

    -- Payment
    amount_due DECIMAL(10, 2) NOT NULL,
    host_pays BOOLEAN DEFAULT false,
    paid_at TIMESTAMPTZ,

    -- Payment Info
    stripe_payment_intent TEXT,
    neonet_transaction_id TEXT,
    verification_code TEXT UNIQUE,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_group_guests_reservation ON group_reservation_guests(reservation_id);
CREATE INDEX IF NOT EXISTS idx_group_guests_user ON group_reservation_guests(user_id);

-- =============================================
-- VIP BOTTLES (Catalog)
-- =============================================

CREATE TABLE IF NOT EXISTS vip_bottles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    name TEXT NOT NULL,
    brand TEXT,
    type TEXT,  -- 'vodka', 'whiskey', 'tequila', etc.
    description TEXT,
    image TEXT,

    price DECIMAL(10, 2) NOT NULL,

    is_available BOOLEAN DEFAULT true,

    -- Reference to a generic bottle (for standardization)
    generic_bottle_id UUID,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vip_bottles_available ON vip_bottles(is_available) WHERE is_available = true;

-- =============================================
-- VIP MIXERS (Catalog)
-- =============================================

CREATE TABLE IF NOT EXISTS vip_mixers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    name TEXT NOT NULL,
    description TEXT,
    price DECIMAL(10, 2) NOT NULL DEFAULT 0,

    is_available BOOLEAN DEFAULT true,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- =============================================
-- GROUP RESERVATION BOTTLES
-- =============================================

CREATE TABLE IF NOT EXISTS group_reservation_bottles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL REFERENCES group_reservations(id) ON DELETE CASCADE,
    bottle_id UUID NOT NULL REFERENCES vip_bottles(id),
    quantity INT NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_group_bottles_reservation ON group_reservation_bottles(reservation_id);

-- =============================================
-- GROUP RESERVATION MIXERS
-- =============================================

CREATE TABLE IF NOT EXISTS group_reservation_mixers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL REFERENCES group_reservations(id) ON DELETE CASCADE,
    mixer_id UUID NOT NULL REFERENCES vip_mixers(id),
    quantity INT NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_group_mixers_reservation ON group_reservation_mixers(reservation_id);

-- =============================================
-- VIP LIST RESERVATIONS (Alternative flow)
-- =============================================

CREATE TABLE IF NOT EXISTS vip_list_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- References
    event_id UUID NOT NULL REFERENCES events(id),
    created_by UUID NOT NULL REFERENCES public_users(id),

    -- Host Info
    host_name TEXT NOT NULL,
    host_last_name TEXT NOT NULL,
    host_email TEXT NOT NULL,
    host_phone TEXT,
    host_phone_prefix TEXT DEFAULT '+502',
    host_birth_date DATE,
    host_gender gender_type NOT NULL,

    -- Reservation Details
    reservation_name TEXT,
    description TEXT,
    table_or_bar TEXT NOT NULL CHECK (table_or_bar IN ('table', 'bar')),

    -- Expected Guests
    expected_men INT DEFAULT 0,
    expected_women INT DEFAULT 0,

    -- Pricing
    price_per_person DECIMAL(10, 2),
    male_price DECIMAL(10, 2),
    female_price DECIMAL(10, 2),
    currency TEXT DEFAULT 'GTQ',

    -- Status
    status TEXT DEFAULT 'open' CHECK (status IN ('open', 'closed', 'completed', 'cancelled')),

    -- Codes
    tracking_link_code TEXT NOT NULL UNIQUE,
    host_edit_code TEXT NOT NULL UNIQUE,

    -- Bottles
    bottle_voucher_token TEXT UNIQUE,
    bottles_selected_at TIMESTAMPTZ,
    bottles_selected_by_host BOOLEAN DEFAULT false,
    bottles_redeemed_at TIMESTAMPTZ,
    bottles_redeemed_by UUID REFERENCES organization_workers(id),
    remaining_credit DECIMAL(10, 2) DEFAULT 0,

    -- Deadlines
    payment_deadline TIMESTAMPTZ,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    closed_at TIMESTAMPTZ,
    closed_by UUID REFERENCES organization_workers(id)
);

CREATE INDEX IF NOT EXISTS idx_vip_list_res_event ON vip_list_reservations(event_id);
CREATE INDEX IF NOT EXISTS idx_vip_list_res_tracking ON vip_list_reservations(tracking_link_code);
CREATE INDEX IF NOT EXISTS idx_vip_list_res_edit ON vip_list_reservations(host_edit_code);

-- =============================================
-- VIP LIST GUESTS
-- =============================================

CREATE TABLE IF NOT EXISTS vip_list_guests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL REFERENCES vip_list_reservations(id) ON DELETE CASCADE,
    ticket_id UUID REFERENCES tickets(id),

    -- Guest Info
    name TEXT NOT NULL,
    last_name TEXT NOT NULL,
    email TEXT NOT NULL,
    phone TEXT,
    phone_prefix TEXT DEFAULT '+502',
    gender gender_type NOT NULL,
    birth_date DATE,

    -- RSVP
    rsvp_status TEXT DEFAULT 'confirmed' CHECK (rsvp_status IN ('confirmed', 'declined', 'removed')),
    rsvp_at TIMESTAMPTZ DEFAULT NOW(),
    added_by TEXT DEFAULT 'self' CHECK (added_by IN ('host', 'self')),

    -- Payment
    amount_due DECIMAL(10, 2) NOT NULL,
    paid_at TIMESTAMPTZ,
    payment_status TEXT DEFAULT 'pending',
    payment_error TEXT,
    payment_transaction_id UUID,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vip_list_guests_reservation ON vip_list_guests(reservation_id);

-- =============================================
-- VIP LIST BOTTLES
-- =============================================

CREATE TABLE IF NOT EXISTS vip_list_bottles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL REFERENCES vip_list_reservations(id) ON DELETE CASCADE,
    bottle_id UUID NOT NULL REFERENCES vip_bottles(id),
    quantity INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vip_list_bottles_reservation ON vip_list_bottles(reservation_id);

-- =============================================
-- GUEST LIST TYPES
-- =============================================

CREATE TABLE IF NOT EXISTS guest_list_types (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,

    name TEXT NOT NULL,
    description TEXT,

    max_capacity INT,
    current_count INT DEFAULT 0 CHECK (current_count >= 0),

    allowed_gender gender_type,  -- NULL = any gender
    is_active BOOLEAN DEFAULT true,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_guest_list_types_event ON guest_list_types(event_id);

-- =============================================
-- GUEST LIST SIGNUPS
-- =============================================

CREATE TABLE IF NOT EXISTS guest_list_signups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    guest_list_type_id UUID NOT NULL REFERENCES guest_list_types(id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events(id),
    user_id UUID REFERENCES public_users(id),
    ticket_id UUID REFERENCES tickets(id),

    -- Guest Info
    name TEXT NOT NULL,
    last_name TEXT NOT NULL,
    email TEXT NOT NULL,
    phone TEXT,
    phone_prefix TEXT DEFAULT '+502',
    gender gender_type NOT NULL,
    guest_count INT DEFAULT 0 CHECK (guest_count >= 0),

    -- Status
    status TEXT DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled')),
    verification_code TEXT NOT NULL UNIQUE,

    -- Processing
    processed_at TIMESTAMPTZ,
    processed_by UUID REFERENCES organization_workers(id),
    reject_reason TEXT,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_guest_list_signups_type ON guest_list_signups(guest_list_type_id);
CREATE INDEX IF NOT EXISTS idx_guest_list_signups_event ON guest_list_signups(event_id);
CREATE INDEX IF NOT EXISTS idx_guest_list_signups_code ON guest_list_signups(verification_code);

-- =============================================
-- TRANSACTIONS (Full transaction details)
-- =============================================

CREATE TABLE IF NOT EXISTS transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_number TEXT NOT NULL UNIQUE,

    -- Type & Status
    transaction_type transaction_type NOT NULL,
    status transaction_status DEFAULT 'pending',

    -- References
    event_id UUID NOT NULL REFERENCES events(id),
    user_id UUID NOT NULL REFERENCES public_users(id),
    order_id UUID REFERENCES orders(id),
    group_reservation_id UUID REFERENCES group_reservations(id),
    group_guest_id UUID REFERENCES group_reservation_guests(id),

    -- Amounts
    gross_amount DECIMAL(12, 2) NOT NULL CHECK (abs(gross_amount) <= 1000000),
    platform_fee_percent DECIMAL(5, 2) NOT NULL,
    platform_fee_amount DECIMAL(12, 2) NOT NULL,
    stripe_fee_percent DECIMAL(5, 2) DEFAULT 2.90,
    stripe_fee_fixed DECIMAL(5, 2) DEFAULT 0.30,
    stripe_fee_amount DECIMAL(10, 2) DEFAULT 0,
    net_to_venue DECIMAL(12, 2) NOT NULL,
    currency TEXT DEFAULT 'GTQ',

    -- Payment Gateway
    payment_gateway payment_gateway_type,

    -- Stripe fields
    stripe_payment_intent TEXT,
    stripe_charge_id TEXT,
    stripe_session_id TEXT,
    stripe_transfer_id TEXT,
    stripe_balance_transaction TEXT,

    -- NeoNet fields
    neonet_transaction_id TEXT,
    neonet_authorization_code TEXT,
    neonet_reference TEXT,

    -- Payer Info
    payer_name TEXT,
    payer_email TEXT,
    payer_phone TEXT,

    -- Refund tracking
    original_transaction_id UUID REFERENCES transactions(id),
    refund_reason TEXT,

    -- Metadata
    metadata JSONB DEFAULT '{}',
    internal_notes TEXT,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    captured_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_transactions_event ON transactions(event_id);
CREATE INDEX IF NOT EXISTS idx_transactions_user ON transactions(user_id);
CREATE INDEX IF NOT EXISTS idx_transactions_order ON transactions(order_id);
CREATE INDEX IF NOT EXISTS idx_transactions_number ON transactions(transaction_number);
CREATE INDEX IF NOT EXISTS idx_transactions_date ON transactions(created_at);
CREATE INDEX IF NOT EXISTS idx_transactions_status ON transactions(status);

-- =============================================
-- TRANSACTION LINE ITEMS
-- =============================================

CREATE TABLE IF NOT EXISTS transaction_line_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,

    -- Item
    item_type TEXT NOT NULL CHECK (item_type IN ('ticket', 'bottle', 'mixer', 'service_fee', 'tip', 'other')),
    item_id UUID,
    item_name TEXT NOT NULL,
    item_description TEXT,

    -- Pricing
    quantity INT NOT NULL DEFAULT 1,
    unit_price DECIMAL(10, 2) NOT NULL,
    total_price DECIMAL(10, 2) NOT NULL,

    -- For tickets
    gender gender_type,

    -- Metadata
    metadata JSONB DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_transaction_items_tx ON transaction_line_items(transaction_id);

-- =============================================
-- PAYMENT TRANSACTIONS (NeoNet/Cybersource specific)
-- =============================================

CREATE TABLE IF NOT EXISTS payment_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- References
    guest_id UUID NOT NULL,  -- Can be group_reservation_guests or vip_list_guests
    reservation_id UUID NOT NULL,  -- group_reservations or vip_list_reservations
    event_id UUID REFERENCES events(id),

    -- Type
    payment_type TEXT NOT NULL DEFAULT 'split' CHECK (payment_type IN ('split', 'full')),

    -- Venue Payment
    venue_amount DECIMAL(10, 2) NOT NULL,
    venue_transaction_id TEXT,
    venue_auth_code TEXT,
    venue_reference_number TEXT,
    venue_status TEXT,
    venue_reason_code TEXT,

    -- Platform Fee Payment
    fee_amount DECIMAL(10, 2) NOT NULL,
    fee_transaction_id TEXT,
    fee_auth_code TEXT,
    fee_reference_number TEXT,
    fee_status TEXT,
    fee_reason_code TEXT,

    -- Total
    total_amount DECIMAL(10, 2) NOT NULL,
    currency TEXT DEFAULT 'GTQ',

    -- Card Info (tokenized/masked)
    card_last4 TEXT,
    card_type TEXT,
    card_brand TEXT,

    -- Status
    status TEXT DEFAULT 'pending' CHECK (status IN ('pending', 'authorized', 'captured', 'failed', 'reversed', 'refunded')),
    error_message TEXT,

    -- Security
    customer_ip TEXT,
    device_fingerprint TEXT,

    -- Metadata
    metadata JSONB DEFAULT '{}',

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    authorized_at TIMESTAMPTZ,
    captured_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_payment_tx_guest ON payment_transactions(guest_id);
CREATE INDEX IF NOT EXISTS idx_payment_tx_reservation ON payment_transactions(reservation_id);
CREATE INDEX IF NOT EXISTS idx_payment_tx_event ON payment_transactions(event_id);

-- Add FK to vip_list_guests
ALTER TABLE vip_list_guests ADD CONSTRAINT fk_vip_guests_payment_tx
    FOREIGN KEY (payment_transaction_id) REFERENCES payment_transactions(id);

-- =============================================
-- PAYMENT AUDIT LOG
-- =============================================

CREATE TABLE IF NOT EXISTS payment_audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID REFERENCES payment_transactions(id),

    -- Operation
    operation TEXT NOT NULL,
    target TEXT NOT NULL,

    -- Request/Response
    request_data JSONB,
    response_data JSONB,

    -- Result
    success BOOLEAN NOT NULL,
    error_message TEXT,

    -- Gateway Info
    gateway_transaction_id TEXT,
    gateway_reason_code TEXT,
    gateway_decision TEXT,

    -- Timestamp
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_payment_audit_tx ON payment_audit_log(transaction_id);

-- =============================================
-- REVENUE SUMMARIES
-- =============================================

-- Daily Revenue Summary
CREATE TABLE IF NOT EXISTS daily_revenue_summary (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    date DATE NOT NULL UNIQUE,

    -- Totals
    total_transactions INT DEFAULT 0,
    total_gross_amount DECIMAL(14, 2) DEFAULT 0,
    total_platform_fee DECIMAL(14, 2) DEFAULT 0,
    total_stripe_fee DECIMAL(14, 2) DEFAULT 0,
    total_net_to_venue DECIMAL(14, 2) DEFAULT 0,

    -- Individual Tickets
    individual_ticket_count INT DEFAULT 0,
    individual_ticket_gross DECIMAL(14, 2) DEFAULT 0,
    individual_ticket_platform_fee DECIMAL(14, 2) DEFAULT 0,
    individual_ticket_net_venue DECIMAL(14, 2) DEFAULT 0,

    -- Group Organizers
    group_organizer_count INT DEFAULT 0,
    group_organizer_gross DECIMAL(14, 2) DEFAULT 0,
    group_organizer_platform_fee DECIMAL(14, 2) DEFAULT 0,
    group_organizer_net_venue DECIMAL(14, 2) DEFAULT 0,

    -- Group Guests
    group_guest_count INT DEFAULT 0,
    group_guest_gross DECIMAL(14, 2) DEFAULT 0,
    group_guest_platform_fee DECIMAL(14, 2) DEFAULT 0,
    group_guest_net_venue DECIMAL(14, 2) DEFAULT 0,

    -- Refunds
    refund_count INT DEFAULT 0,
    refund_gross_amount DECIMAL(14, 2) DEFAULT 0,
    refund_platform_fee DECIMAL(14, 2) DEFAULT 0,

    -- Metrics
    unique_customers INT DEFAULT 0,
    tickets_sold INT DEFAULT 0,
    bottles_sold INT DEFAULT 0,
    average_transaction_value DECIMAL(10, 2) DEFAULT 0,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_daily_revenue_date ON daily_revenue_summary(date);

-- Monthly Revenue Summary
CREATE TABLE IF NOT EXISTS monthly_revenue_summary (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    year INT NOT NULL,
    month INT NOT NULL CHECK (month >= 1 AND month <= 12),

    -- Totals
    total_transactions INT DEFAULT 0,
    total_gross_amount DECIMAL(16, 2) DEFAULT 0,
    total_platform_fee DECIMAL(16, 2) DEFAULT 0,
    total_stripe_fee DECIMAL(16, 2) DEFAULT 0,
    total_net_to_venue DECIMAL(16, 2) DEFAULT 0,

    -- By Type
    individual_ticket_count INT DEFAULT 0,
    individual_ticket_gross DECIMAL(16, 2) DEFAULT 0,
    individual_ticket_platform_fee DECIMAL(16, 2) DEFAULT 0,

    group_organizer_count INT DEFAULT 0,
    group_organizer_gross DECIMAL(16, 2) DEFAULT 0,
    group_organizer_platform_fee DECIMAL(16, 2) DEFAULT 0,

    group_guest_count INT DEFAULT 0,
    group_guest_gross DECIMAL(16, 2) DEFAULT 0,
    group_guest_platform_fee DECIMAL(16, 2) DEFAULT 0,

    -- Refunds
    refund_count INT DEFAULT 0,
    refund_gross_amount DECIMAL(16, 2) DEFAULT 0,

    -- Metrics
    unique_customers INT DEFAULT 0,
    total_tickets_sold INT DEFAULT 0,
    total_bottles_sold INT DEFAULT 0,
    average_transaction_value DECIMAL(10, 2) DEFAULT 0,

    -- Growth
    gross_change_vs_prev_month DECIMAL(8, 2),
    transactions_change_vs_prev_month DECIMAL(8, 2),

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(year, month)
);

CREATE INDEX IF NOT EXISTS idx_monthly_revenue_year_month ON monthly_revenue_summary(year, month);

-- Event Revenue Summary
CREATE TABLE IF NOT EXISTS event_revenue_summary (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL UNIQUE REFERENCES events(id) ON DELETE CASCADE,

    -- Totals
    total_transactions INT DEFAULT 0,
    total_gross_amount DECIMAL(14, 2) DEFAULT 0,
    total_platform_fee DECIMAL(14, 2) DEFAULT 0,
    total_net_to_venue DECIMAL(14, 2) DEFAULT 0,

    -- Tickets
    individual_tickets_sold INT DEFAULT 0,
    individual_tickets_gross DECIMAL(14, 2) DEFAULT 0,
    group_tickets_sold INT DEFAULT 0,
    group_tickets_gross DECIMAL(14, 2) DEFAULT 0,

    -- Groups
    total_group_reservations INT DEFAULT 0,
    total_group_guests INT DEFAULT 0,

    -- Bottles
    bottles_sold INT DEFAULT 0,
    bottles_revenue DECIMAL(14, 2) DEFAULT 0,

    -- Customers
    unique_customers INT DEFAULT 0,
    average_transaction_value DECIMAL(10, 2) DEFAULT 0,

    -- Demographics
    male_attendees INT DEFAULT 0,
    female_attendees INT DEFAULT 0,
    other_attendees INT DEFAULT 0,

    -- Check-in
    total_checked_in INT DEFAULT 0,
    check_in_rate DECIMAL(5, 2) DEFAULT 0,

    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_event_revenue_event ON event_revenue_summary(event_id);

-- =============================================
-- AUTHENTICATION TABLES
-- =============================================

-- Verification Codes (for email login)
CREATE TABLE IF NOT EXISTS verification_codes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES public_users(id) ON DELETE CASCADE,

    code TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,

    used BOOLEAN DEFAULT false,
    used_at TIMESTAMPTZ,
    attempts INT DEFAULT 0,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_verification_codes_user ON verification_codes(user_id);
CREATE INDEX IF NOT EXISTS idx_verification_codes_expires ON verification_codes(expires_at);

-- User Sessions
CREATE TABLE IF NOT EXISTS user_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES public_users(id) ON DELETE CASCADE,

    token TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,

    ip_address TEXT,
    user_agent TEXT,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON user_sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_user_sessions_token ON user_sessions(token);

-- User Access Tokens
CREATE TABLE IF NOT EXISTS user_access_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES public_users(id) ON DELETE CASCADE,

    token TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_user_access_tokens_user ON user_access_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_user_access_tokens_token ON user_access_tokens(token);

-- User Refresh Tokens
CREATE TABLE IF NOT EXISTS user_refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES public_users(id) ON DELETE CASCADE,

    token_hash TEXT NOT NULL,
    family_id TEXT NOT NULL,

    expires_at TIMESTAMPTZ NOT NULL,
    is_revoked BOOLEAN DEFAULT false,

    device_info TEXT,
    ip_address TEXT,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_refresh_tokens_user ON user_refresh_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_user_refresh_tokens_family ON user_refresh_tokens(family_id);

-- =============================================
-- NOTIFICATIONS
-- =============================================

-- Staff Notifications
CREATE TABLE IF NOT EXISTS staff_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Type
    type TEXT NOT NULL,  -- 'new_order', 'new_reservation', 'payment_received', etc.

    -- References
    order_id UUID REFERENCES orders(id),
    reservation_id UUID REFERENCES group_reservations(id),
    event_id UUID REFERENCES events(id),
    guest_list_signup_id UUID REFERENCES guest_list_signups(id),

    -- Info
    user_name TEXT,
    user_email TEXT,
    quantity INT,
    total DECIMAL(10, 2),

    -- Status
    status TEXT DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'viewed')),

    -- Processing
    processed_at TIMESTAMPTZ,
    processed_by UUID REFERENCES organization_workers(id),
    process_note TEXT,

    -- Timestamp
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_staff_notifications_status ON staff_notifications(status);
CREATE INDEX IF NOT EXISTS idx_staff_notifications_date ON staff_notifications(created_at);

-- Staff Push Tokens
CREATE TABLE IF NOT EXISTS staff_push_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    employee_id UUID NOT NULL REFERENCES organization_workers(id) ON DELETE CASCADE,

    push_token TEXT NOT NULL,
    device_type TEXT DEFAULT 'unknown',

    is_active BOOLEAN DEFAULT true,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_staff_push_tokens_employee ON staff_push_tokens(employee_id);

-- =============================================
-- USER SPENDING TRACKING
-- =============================================

CREATE TABLE IF NOT EXISTS user_venue_spending (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES public_users(id) ON DELETE CASCADE,

    total_spent DECIMAL(12, 2) DEFAULT 0,
    total_tickets INT DEFAULT 0,
    last_purchase_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(user_id)
);

CREATE INDEX IF NOT EXISTS idx_user_spending_user ON user_venue_spending(user_id);

-- =============================================
-- ROW LEVEL SECURITY (RLS)
-- =============================================

-- Enable RLS on all tables
ALTER TABLE public_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_workers ENABLE ROW LEVEL SECURITY;
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ticket_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_vip_ticket_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE group_reservation_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE group_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE group_reservation_guests ENABLE ROW LEVEL SECURITY;
ALTER TABLE vip_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE vip_mixers ENABLE ROW LEVEL SECURITY;
ALTER TABLE group_reservation_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE group_reservation_mixers ENABLE ROW LEVEL SECURITY;
ALTER TABLE vip_list_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE vip_list_guests ENABLE ROW LEVEL SECURITY;
ALTER TABLE vip_list_bottles ENABLE ROW LEVEL SECURITY;
ALTER TABLE guest_list_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE guest_list_signups ENABLE ROW LEVEL SECURITY;
ALTER TABLE transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE transaction_line_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE daily_revenue_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE monthly_revenue_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_revenue_summary ENABLE ROW LEVEL SECURITY;
ALTER TABLE verification_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_access_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_push_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_venue_spending ENABLE ROW LEVEL SECURITY;

-- Service role has full access (API uses service_key)
CREATE POLICY "Service role full access" ON public_users FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON roles FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON organization_workers FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON events FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON ticket_types FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON event_vip_ticket_types FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON orders FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON tickets FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON group_reservation_status FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON group_reservations FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON group_reservation_guests FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON vip_bottles FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON vip_mixers FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON group_reservation_bottles FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON group_reservation_mixers FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON vip_list_reservations FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON vip_list_guests FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON vip_list_bottles FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON guest_list_types FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON guest_list_signups FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON transactions FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON transaction_line_items FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON payment_transactions FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON payment_audit_log FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON daily_revenue_summary FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON monthly_revenue_summary FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON event_revenue_summary FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON verification_codes FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON user_sessions FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON user_access_tokens FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON user_refresh_tokens FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON staff_notifications FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON staff_push_tokens FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY "Service role full access" ON user_venue_spending FOR ALL USING (true) WITH CHECK (true);

-- =============================================
-- FUNCTIONS
-- =============================================

-- Auto-update updated_at
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Apply triggers
CREATE TRIGGER update_public_users_updated_at BEFORE UPDATE ON public_users FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_org_workers_updated_at BEFORE UPDATE ON organization_workers FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_events_updated_at BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_ticket_types_updated_at BEFORE UPDATE ON ticket_types FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_orders_updated_at BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_group_reservations_updated_at BEFORE UPDATE ON group_reservations FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_group_guests_updated_at BEFORE UPDATE ON group_reservation_guests FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_vip_list_reservations_updated_at BEFORE UPDATE ON vip_list_reservations FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_transactions_updated_at BEFORE UPDATE ON transactions FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_payment_tx_updated_at BEFORE UPDATE ON payment_transactions FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_daily_revenue_updated_at BEFORE UPDATE ON daily_revenue_summary FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_monthly_revenue_updated_at BEFORE UPDATE ON monthly_revenue_summary FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_event_revenue_updated_at BEFORE UPDATE ON event_revenue_summary FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_verification_codes_updated_at BEFORE UPDATE ON verification_codes FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_user_spending_updated_at BEFORE UPDATE ON user_venue_spending FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_guest_list_types_updated_at BEFORE UPDATE ON guest_list_types FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
CREATE TRIGGER update_staff_push_tokens_updated_at BEFORE UPDATE ON staff_push_tokens FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Generate order number
CREATE OR REPLACE FUNCTION generate_order_number()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.order_number IS NULL THEN
        NEW.order_number = 'ORD-' || TO_CHAR(NOW(), 'YYYYMMDD') || '-' || LPAD(FLOOR(RANDOM() * 100000)::TEXT, 5, '0');
    END IF;
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE TRIGGER generate_order_number_trigger BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION generate_order_number();

-- Generate transaction number
CREATE OR REPLACE FUNCTION generate_transaction_number()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.transaction_number IS NULL THEN
        NEW.transaction_number = 'TXN-' || TO_CHAR(NOW(), 'YYYYMMDD') || '-' || LPAD(FLOOR(RANDOM() * 100000)::TEXT, 5, '0');
    END IF;
    RETURN NEW;
END;
$$ language 'plpgsql';

CREATE TRIGGER generate_transaction_number_trigger BEFORE INSERT ON transactions FOR EACH ROW EXECUTE FUNCTION generate_transaction_number();

-- =============================================
-- DONE!
-- =============================================
-- Venue template created successfully.
--
-- Total tables: 35+
-- =============================================
