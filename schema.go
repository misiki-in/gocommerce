package gocommerce

// coreMigrations returns the engine's own schema, applied before any module's.
//
// Migrations are forward-only and append-only: once an ID has shipped, its SQL
// is frozen, and a correction is a new migration. Editing a released migration
// would leave every existing database in a state no future version knows how
// to reason about.
func coreMigrations() []Migration {
	return []Migration{
		{ID: "0001_catalog", SQL: migration0001Catalog},
		{ID: "0002_cart", SQL: migration0002Cart},
		{ID: "0003_orders_and_outbox", SQL: migration0003OrdersAndOutbox},
		{ID: "0004_fulfillment", SQL: migration0004Fulfillment},
		{ID: "0005_superusers", SQL: migration0005Superusers},
		{ID: "0006_merchandising", SQL: migration0006Merchandising},
		{ID: "0007_weight_units", SQL: migration0007WeightUnits},
		{ID: "0008_categories", SQL: migration0008Categories},
		{ID: "0009_variant_media", SQL: migration0009VariantMedia},
		{ID: "0010_cost_and_tax", SQL: migration0010CostAndTax},
		{ID: "0011_customs", SQL: migration0011Customs},
		{ID: "0012_continue_selling", SQL: migration0012ContinueSelling},
		{ID: "0013_roles", SQL: migration0013Roles},
		{ID: "0013_taxonomy_attributes", SQL: migration0013TaxonomyAttributes},
		{ID: "0014_fulfillment_carrier", SQL: migration0014FulfillmentCarrier},
		{ID: "0015_discounts", SQL: migration0015Discounts},
		{ID: "0016_taxes", SQL: migration0016Taxes},
		{ID: "0017_locations", SQL: migration0017Locations},
		{ID: "0018_invitations", SQL: migration0018Invitations},
	}
}

// M1 — catalog. A product is the merchandising concept; a variant is the
// sellable unit. Everything downstream (cart lines, order lines, inventory)
// references a variant, so a product with no options still gets exactly one
// default variant and the simple case never becomes a special case.
const migration0001Catalog = `
CREATE TABLE products (
    id          bigserial   PRIMARY KEY,
    slug        text        NOT NULL UNIQUE,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT 'draft'
                            CHECK (status IN ('draft', 'active', 'archived')),
    currency    text        NOT NULL,
    metadata    jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX products_status_idx ON products (status, id DESC);

CREATE TABLE product_options (
    id         bigserial PRIMARY KEY,
    product_id bigint    NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    name       text      NOT NULL,
    position   integer   NOT NULL DEFAULT 0,
    UNIQUE (product_id, name)
);
CREATE INDEX product_options_product_idx ON product_options (product_id, position);

CREATE TABLE product_option_values (
    id        bigserial PRIMARY KEY,
    option_id bigint    NOT NULL REFERENCES product_options (id) ON DELETE CASCADE,
    value     text      NOT NULL,
    position  integer   NOT NULL DEFAULT 0,
    UNIQUE (option_id, value)
);
CREATE INDEX product_option_values_option_idx ON product_option_values (option_id, position);

CREATE TABLE variants (
    id                     bigserial   PRIMARY KEY,
    product_id             bigint      NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    sku                    text        NOT NULL UNIQUE,
    barcode                text,
    price_minor            bigint      NOT NULL CHECK (price_minor >= 0),
    compare_at_price_minor bigint      CHECK (compare_at_price_minor IS NULL OR compare_at_price_minor >= 0),
    -- Inventory lives on the variant because quantity belongs to the sellable
    -- SKU. available = on_hand - reserved, and neither may go negative: the
    -- constraints are the last line of defence behind the reservation service.
    stock_on_hand          integer     NOT NULL DEFAULT 0 CHECK (stock_on_hand >= 0),
    stock_reserved         integer     NOT NULL DEFAULT 0 CHECK (stock_reserved >= 0),
    track_inventory        boolean     NOT NULL DEFAULT true,
    active                 boolean     NOT NULL DEFAULT true,
    weight_grams           integer     CHECK (weight_grams IS NULL OR weight_grams >= 0),
    position               integer     NOT NULL DEFAULT 0,
    -- option_key is the variant's option selection, normalised: the variant's
    -- option_value_ids sorted and joined. A unique index over it is what makes
    -- "no two variants may be Color=Black + Size=M" enforceable in the
    -- database rather than hopeful in the service. A product with no options
    -- has one variant whose key is '', so the same index also guarantees the
    -- single default variant.
    option_key             text        NOT NULL DEFAULT '',
    metadata               jsonb       NOT NULL DEFAULT '{}',
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT variants_reserved_within_on_hand CHECK (stock_reserved <= stock_on_hand)
);
CREATE UNIQUE INDEX variants_product_option_key_idx ON variants (product_id, option_key);
CREATE INDEX variants_product_idx ON variants (product_id, position, id);

CREATE TABLE variant_option_values (
    variant_id      bigint NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
    option_value_id bigint NOT NULL REFERENCES product_option_values (id) ON DELETE RESTRICT,
    PRIMARY KEY (variant_id, option_value_id)
);
CREATE INDEX variant_option_values_value_idx ON variant_option_values (option_value_id);
`

// M2 — carts. A cart is guest-owned: its token is the only credential a
// shopper ever has, and no row here references a customer, because guest
// checkout is a permanent guarantee rather than a stage the project grows out
// of.
const migration0002Cart = `
CREATE TABLE carts (
    id         bigserial   PRIMARY KEY,
    token      text        NOT NULL UNIQUE,
    currency   text        NOT NULL,
    status     text        NOT NULL DEFAULT 'open'
                           CHECK (status IN ('open', 'converted', 'abandoned')),
    email      text,
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX carts_expiry_idx ON carts (expires_at) WHERE status = 'open';

CREATE TABLE cart_line_items (
    id         bigserial   PRIMARY KEY,
    cart_id    bigint      NOT NULL REFERENCES carts (id) ON DELETE CASCADE,
    -- Cascade, not restrict: a cart line for a deleted variant cannot be
    -- bought, so it should disappear with it. Restricting instead would let
    -- one abandoned guest cart block an operator from ever removing a
    -- product. Order lines are the opposite case — they keep their snapshot
    -- and merely lose the reference.
    variant_id bigint      NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
    quantity   integer     NOT NULL CHECK (quantity > 0),
    -- The price when the line was added. Checkout compares it against the
    -- authoritative price and refuses to silently reprice a confirmed order.
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cart_id, variant_id)
);
CREATE INDEX cart_line_items_cart_idx ON cart_line_items (cart_id, id);
`

// M3 — orders, idempotency and the transactional outbox.
const migration0003OrdersAndOutbox = `
CREATE TABLE orders (
    id       bigserial PRIMARY KEY,
    number   text      NOT NULL UNIQUE,
    -- The shopper's handle on their own order. Guest checkout means there is
    -- no account to log into, so this token is how an order is looked up.
    access_token text  NOT NULL,
    status   text      NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'confirmed', 'shipped', 'delivered', 'cancelled')),
    payment_status text NOT NULL DEFAULT 'pending'
                       CHECK (payment_status IN ('pending', 'paid', 'failed', 'refunded')),
    payment_provider text NOT NULL,
    payment_reference text,
    currency text        NOT NULL,
    subtotal_minor bigint NOT NULL CHECK (subtotal_minor >= 0),
    shipping_minor bigint NOT NULL DEFAULT 0 CHECK (shipping_minor >= 0),
    discount_minor bigint NOT NULL DEFAULT 0 CHECK (discount_minor >= 0),
    total_minor    bigint NOT NULL CHECK (total_minor >= 0),
    -- The customer as they were at checkout. Historical orders never depend on
    -- mutable customer records for their legal or operational meaning.
    email    text        NOT NULL,
    phone    text,
    name     text,
    address  jsonb       NOT NULL DEFAULT '{}',
    lang     text        NOT NULL DEFAULT 'en',
    metadata jsonb       NOT NULL DEFAULT '{}',
    -- Set while stock is reserved but not yet committed to the sale; the
    -- sweeper uses it to release inventory an abandoned payment is holding.
    reservation_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_status_idx ON orders (status, id DESC);
CREATE INDEX orders_created_idx ON orders (created_at DESC);
CREATE INDEX orders_unpaid_idx ON orders (reservation_expires_at)
    WHERE status = 'pending' AND payment_status = 'pending';

CREATE TABLE order_lines (
    id         bigserial PRIMARY KEY,
    order_id   bigint    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    -- Nullable on purpose: an order line is an immutable snapshot and must
    -- stay readable after the product or variant it came from is deleted.
    product_id bigint    REFERENCES products (id) ON DELETE SET NULL,
    variant_id bigint    REFERENCES variants (id) ON DELETE SET NULL,
    sku        text      NOT NULL,
    title      text      NOT NULL,
    variant_label text   NOT NULL DEFAULT '',
    quantity   integer   NOT NULL CHECK (quantity > 0),
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor >= 0),
    total_minor      bigint NOT NULL CHECK (total_minor >= 0),
    metadata   jsonb     NOT NULL DEFAULT '{}'
);
CREATE INDEX order_lines_order_idx ON order_lines (order_id, id);

-- Same key, same operation, same answer. The unique key is what stops a
-- double-tapped checkout becoming two orders.
CREATE TABLE idempotency_keys (
    id           bigserial   PRIMARY KEY,
    scope        text        NOT NULL,
    key          text        NOT NULL,
    request_hash text        NOT NULL,
    order_id     bigint      REFERENCES orders (id) ON DELETE CASCADE,
    response     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, key)
);

-- The transactional outbox: an event is written in the same transaction as the
-- state change that caused it, so a crash between commit and publish cannot
-- lose it.
CREATE TABLE outbox_events (
    id             bigserial   PRIMARY KEY,
    event_id       uuid        NOT NULL UNIQUE,
    event_name     text        NOT NULL,
    event_version  integer     NOT NULL DEFAULT 1,
    aggregate_type text        NOT NULL,
    aggregate_id   bigint      NOT NULL,
    payload        jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    available_at   timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text,
    dead           boolean     NOT NULL DEFAULT false
);
-- The dispatcher's claim query rides this index; it covers only unpublished
-- rows, so a table of delivered history costs nothing to skip.
CREATE INDEX outbox_unpublished_idx ON outbox_events (available_at, id)
    WHERE published_at IS NULL AND NOT dead;
CREATE INDEX outbox_aggregate_idx ON outbox_events (aggregate_type, aggregate_id, id);
`

// M4 — fulfillment.
const migration0004Fulfillment = `
CREATE TABLE fulfillments (
    id         bigserial   PRIMARY KEY,
    order_id   bigint      NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    provider   text        NOT NULL,
    tracking   text        NOT NULL DEFAULT '',
    label_url  text        NOT NULL DEFAULT '',
    status     text        NOT NULL DEFAULT 'shipped'
                           CHECK (status IN ('shipped', 'delivered', 'cancelled')),
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX fulfillments_order_idx ON fulfillments (order_id, id);
`

// M5 — superusers: the operators who sign in to the admin panel with an email
// and a password.
//
// This is an *operator* table, not a customer table. Nothing in the commerce
// schema references it, and no order, cart or checkout path reads it, so
// guest checkout (D22) stays a property of the engine rather than a mode.
const migration0005Superusers = `
CREATE TABLE superusers (
    id            bigserial   PRIMARY KEY,
    email         text        NOT NULL UNIQUE,
    -- Self-describing PBKDF2: "pbkdf2-sha256$<iterations>$<salt>$<key>". The
    -- cost lives in the row, so raising it later needs no migration and no
    -- flag day; old hashes keep verifying against their own parameters.
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Only the SHA-256 of a session token is stored. A leaked database therefore
-- yields no usable session, and the token itself exists solely in the
-- operator's browser.
CREATE TABLE superuser_sessions (
    token_hash   text        PRIMARY KEY,
    superuser_id bigint      NOT NULL REFERENCES superusers (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX superuser_sessions_user_idx ON superuser_sessions (superuser_id);
CREATE INDEX superuser_sessions_expiry_idx ON superuser_sessions (expires_at);
`

// M6 — merchandising: the product attributes an operator organises a catalog
// by, and the images a storefront renders.
//
// These are the fields every commerce admin has and this one did not: type,
// vendor, tags, collections and media. They are merchandising metadata — the
// commerce state machine does not read any of them, which is why they can be
// added without touching checkout, inventory or orders.
const migration0006Merchandising = `
ALTER TABLE products
    ADD COLUMN product_type text   NOT NULL DEFAULT '',
    ADD COLUMN vendor       text   NOT NULL DEFAULT '',
    -- An array rather than a join table: tags are free text with no identity
    -- of their own, and nothing needs to rename one everywhere at once.
    ADD COLUMN tags         text[] NOT NULL DEFAULT '{}',
    -- The storefront's <title> and meta description. Empty means "derive it
    -- from the title", which is the storefront's call, not the engine's.
    ADD COLUMN seo_title       text NOT NULL DEFAULT '',
    ADD COLUMN seo_description text NOT NULL DEFAULT '';

CREATE INDEX products_type_idx   ON products (product_type) WHERE product_type <> '';
CREATE INDEX products_vendor_idx ON products (vendor)       WHERE vendor <> '';
CREATE INDEX products_tags_idx   ON products USING gin (tags);

-- Collections are a named, ordered grouping an operator curates by hand.
-- Rule-based ("smart") collections are deliberately out: they need a query
-- language, and a hand-picked list is what a small catalog actually uses.
CREATE TABLE collections (
    id          bigserial   PRIMARY KEY,
    slug        text        NOT NULL UNIQUE,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    position    integer     NOT NULL DEFAULT 0,
    metadata    jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX collections_position_idx ON collections (position, id);

CREATE TABLE product_collections (
    product_id    bigint  NOT NULL REFERENCES products (id)    ON DELETE CASCADE,
    collection_id bigint  NOT NULL REFERENCES collections (id) ON DELETE CASCADE,
    position      integer NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, collection_id)
);
CREATE INDEX product_collections_collection_idx
    ON product_collections (collection_id, position, product_id);

-- Media is a store-wide library, not a per-product list. That is what makes
-- "select existing" possible: the same photograph attaches to six products
-- and is stored, and re-encoded, once.
--
-- Rows, not blobs. The engine records where a file is and what it is; the
-- bytes live behind a storage seam, which is what lets a local directory today
-- become object storage later without a schema change.
CREATE TABLE media (
    id   bigserial PRIMARY KEY,
    -- image | video | model — a closed set, because the panel renders each
    -- differently and an unknown kind has no sensible presentation.
    kind text      NOT NULL CHECK (kind IN ('image', 'video', 'model')),
    url  text      NOT NULL,
    -- Empty for media referenced by URL. Set for files this store holds, so
    -- deleting the row can delete the file — and so nothing is ever deleted
    -- from someone else's server.
    storage_key text NOT NULL DEFAULT '',
    filename    text NOT NULL DEFAULT '',
    mime        text NOT NULL DEFAULT '',
    size_bytes  bigint NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    -- Known for images, null for video and models the engine does not decode.
    width  integer CHECK (width  IS NULL OR width  > 0),
    height integer CHECK (height IS NULL OR height > 0),
    alt    text    NOT NULL DEFAULT '',
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX media_kind_idx ON media (kind, id DESC);
CREATE INDEX media_recent_idx ON media (id DESC);

-- What a product shows, in the order it shows it.
CREATE TABLE product_media (
    product_id bigint  NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    -- RESTRICT, not CASCADE: deleting a library item that six products still
    -- display should be refused, not silently strip them all.
    media_id   bigint  NOT NULL REFERENCES media (id) ON DELETE RESTRICT,
    -- Nullable: media belongs to the product, or to one variant of it.
    -- SET NULL so removing a variant does not delete a picture the product
    -- still wants.
    variant_id bigint  REFERENCES variants (id) ON DELETE SET NULL,
    position   integer NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, media_id)
);
CREATE INDEX product_media_product_idx ON product_media (product_id, position, media_id);
CREATE INDEX product_media_media_idx   ON product_media (media_id);
CREATE INDEX product_media_variant_idx ON product_media (variant_id) WHERE variant_id IS NOT NULL;
`

// M7 — the unit a weight was entered in.
//
// Weight follows money's rule exactly: one canonical stored value, plus the
// unit it should be shown in. `weight_grams` stays the single source of truth,
// because a carrier API wants a real mass and a database that stores "2.5"
// without saying of what is a database nobody can query. `weight_unit` is
// presentation, the way a currency code is — the number is the fact, the unit
// is how a person reads it.
//
// Storing the entered unit rather than only grams is what keeps the round trip
// honest: an operator who types 2.5 kg should see 2.5 kg when they come back,
// not 2500 g, and not 2.5000000001 from a float that went there and back.
const migration0007WeightUnits = `
ALTER TABLE variants
    ADD COLUMN weight_unit text NOT NULL DEFAULT 'g'
        CHECK (weight_unit IN ('g', 'kg', 'oz', 'lb'));
`

// M8 — the product category tree.
//
// A category is not a collection, and the difference is worth stating because
// the two look alike from a distance. A collection is a curated list: "New in",
// six things somebody picked, and a product belongs to as many as an operator
// likes. A category is where a product *sits* in a taxonomy — one place, with a
// parent — which is what a marketplace feed, a tax rule and a shipping profile
// all need to be able to ask. Being singular is the point: "this is a shirt" has
// one answer, and a product filed under both Shirts and Trousers is a data
// error, not a merchandising decision.
//
// Hence the adjacency list plus a single nullable `products.category_id`, rather
// than another join table. Adjacency (a parent pointer) over a materialised path
// or nested sets because the tree is small and edited by hand: a path column
// would have to be rewritten across a whole subtree on every rename, and the one
// query that needs ancestors is a recursive CTE that PostgreSQL runs in
// microseconds at this size.
const migration0008Categories = `
CREATE TABLE categories (
    id bigserial PRIMARY KEY,
    -- NULL is a root. RESTRICT because deleting "Apparel" must not silently
    -- take "Shirts" and "Trousers" with it — the operator is told what is in
    -- the way and moves it first.
    parent_id bigint REFERENCES categories (id) ON DELETE RESTRICT,
    slug      text    NOT NULL UNIQUE,
    title     text    NOT NULL,
    position  integer NOT NULL DEFAULT 0,
    metadata  jsonb   NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Catches the one-node cycle. Longer cycles are unreachable in SQL and are
    -- refused in Go, where the proposed parent's ancestry is walked first.
    CONSTRAINT categories_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX categories_parent_idx ON categories (parent_id, position, id);

ALTER TABLE products
    -- RESTRICT again, and for the same reason as media: deleting a category
    -- that forty products are filed under should be refused with a count, not
    -- quietly uncategorise all forty where nobody will notice.
    ADD COLUMN category_id bigint REFERENCES categories (id) ON DELETE RESTRICT;
CREATE INDEX products_category_idx ON products (category_id) WHERE category_id IS NOT NULL;
`

// M9 — one image per variant.
//
// `product_media.variant_id` has existed since M6 but nothing enforced how many
// rows could point at one variant, and nothing wrote it. Both are fixed here and
// in media.go: a variant shows exactly one of the product's images, which is
// what a storefront swaps to when a shopper picks a colour, and what a feed
// wants when it lists each variant as its own item.
//
// The index is partial because the common case is NULL — most media belongs to
// the product rather than to a variant, and a full unique index would allow
// exactly one unassigned row per product.
const migration0009VariantMedia = `
CREATE UNIQUE INDEX product_media_variant_key
    ON product_media (variant_id) WHERE variant_id IS NOT NULL;
`

// M10 — cost and taxability, the two things a price needs beside it.
//
// Cost is what the item cost the store, and it is the number every margin
// calculation starts from — Shopify's pricing card shows profit and margin
// derived from it, and neither can be computed from a price alone. It is
// nullable because "we have not recorded a cost" is different from "it cost
// nothing", and a zero would quietly report a 100% margin.
//
// `taxable` sits on the variant rather than the product because it is a
// property of the thing being sold: a gift card and a shirt can share a
// product's shape and not its tax treatment. It defaults to true, which is the
// answer for almost everything and the safe one to be wrong about — charging
// tax that is later refunded is recoverable; not charging it is not.
const migration0010CostAndTax = `
ALTER TABLE variants
    ADD COLUMN cost_minor bigint CHECK (cost_minor IS NULL OR cost_minor >= 0),
    ADD COLUMN taxable boolean NOT NULL DEFAULT true;
`

// M11 — customs: where a thing was made, and what a customs officer calls it.
//
// Both belong to the variant rather than the product, for M10's reason: they
// describe the item in the box. A shirt cut in Portugal and the same shirt cut
// in Vietnam are one product and two origins, and a carrier's paperwork asks
// per line, not per listing.
//
// Text rather than an enum. The origin is an ISO 3166-1 alpha-2 code, which
// changes when countries do; the HS code is 6 to 10 digits whose length depends
// on the importing country, so the tariff number for the same shirt is not the
// same string everywhere. Both are validated on the way in and stored as typed.
// Empty means "not recorded", which is what almost every domestic store will
// leave them as.
const migration0011Customs = `
ALTER TABLE variants
    ADD COLUMN origin_country text NOT NULL DEFAULT ''
        CHECK (origin_country = '' OR origin_country ~ '^[A-Z]{2}$'),
    ADD COLUMN hs_code text NOT NULL DEFAULT ''
        CHECK (hs_code = '' OR hs_code ~ '^[0-9]{6,10}$');
`

// M12 — selling past zero.
//
// `track_inventory` already says whether to count at all. This says what to do
// when the count runs out: refuse the sale, which is the default and what every
// store wants for a thing it has to make; or take the order anyway, which is
// what a store wants for something it drop-ships, back-orders or prints on
// demand.
//
// It is the reason the two stock CHECKs are rewritten rather than dropped. An
// oversold variant is exactly a variant whose reserved has passed its on-hand,
// so the invariant cannot hold unconditionally — but it must still hold for
// every variant that has not opted out, because that is the guarantee the
// reservation path is built on. Conditioning them on the flag keeps the
// database the thing that enforces it, rather than the service remembering to.
const migration0012ContinueSelling = `
ALTER TABLE variants
    ADD COLUMN continue_selling boolean NOT NULL DEFAULT false;

ALTER TABLE variants
    DROP CONSTRAINT variants_reserved_within_on_hand,
    DROP CONSTRAINT variants_stock_on_hand_check,
    ADD CONSTRAINT variants_reserved_within_on_hand
        CHECK (continue_selling OR stock_reserved <= stock_on_hand),
    ADD CONSTRAINT variants_stock_on_hand_check
        CHECK (continue_selling OR stock_on_hand >= 0);
`

// M13 — the choices a taxonomy attribute offers.
//
// A category says which fields it asks of a product, in its own metadata. What
// it does not say is what may be answered: "Color" offers nineteen values and
// they are the same nineteen wherever Color is asked. Written on every category
// that uses it, Shopify's set costs 29MB and puts 400KB of repeated text into
// every page of a category listing; written once, it is 1.5MB and the listing
// carries none of it.
//
// Keyed by handle rather than by an id of our own. The handle is what the
// category's metadata names, it is stable across taxonomy releases, and a store
// that writes its own fields can use whatever handle it likes without asking
// for a row here first — an attribute with no entry simply offers no fixed
// choices, which is a free-text field.
const migration0013TaxonomyAttributes = `
CREATE TABLE taxonomy_attributes (
    handle     text PRIMARY KEY CHECK (handle <> ''),
    label      text NOT NULL CHECK (label <> ''),
    choices    text[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now()
);
`

// M14 — which carrier is actually carrying it.
//
// `provider` already says how the shipment was booked — by hand, or through an
// integration — and that is not the same fact as who is driving the van. A
// store that packs its own boxes books every shipment as "manual" and hands
// them to a different courier depending on the pincode.
//
// It is what turns a tracking number into a link, so it is a column rather than
// a note in metadata: a customer following their parcel is the whole reason the
// number is recorded.
const migration0014FulfillmentCarrier = `
ALTER TABLE fulfillments
    ADD COLUMN carrier text NOT NULL DEFAULT '';
`

// M13 — roles.
//
// Every operator that exists today was the only kind there was: someone with a
// password who could do anything. So the column defaults to owner, and an
// upgrade changes nobody's access. The CHECK is the engine's own list of roles
// (rights.go) written where the database can enforce it — a role it has never
// heard of should not be storable, since RightsOf gives an unknown role nothing
// and a row like that would lock somebody out with no way to see why.
const migration0013Roles = `
ALTER TABLE superusers
    ADD COLUMN role text NOT NULL DEFAULT 'owner'
        CHECK (role IN ('owner', 'manager', 'staff'));
`

// M15 — discounts.
//
// `orders.discount_minor` has existed since M3 and has been written as zero
// ever since: the column was left for this. So this migration adds the rule,
// not the effect — what an order was given is already expressible.
//
// Three tables, because a discount is three different things. `discounts` is a
// rule an operator maintains. `discount_targets` is what a scoped rule points
// at. `order_discounts` is the snapshot, and it exists for the same reason
// `order_lines` snapshots a price rather than pointing at a variant: a finished
// promotion may be deleted, and an order from last winter must still say what
// it was given and what it was called.
const migration0015Discounts = `
CREATE TABLE discounts (
    id            bigserial   PRIMARY KEY,
    -- NULL means automatic: it applies without anybody typing it. Nothing
    -- evaluates those yet (D29); the column is what keeps that additive.
    code          text        CHECK (code IS NULL OR code <> ''),
    title         text        NOT NULL CHECK (title <> ''),
    kind          text        NOT NULL
                              CHECK (kind IN ('percentage', 'fixed', 'free_shipping')),
    -- Basis points: 1000 is 10.00%. An integer, because a percentage of money
    -- is money-adjacent and floats are forbidden anywhere near it.
    value_bp      integer     CHECK (value_bp IS NULL OR (value_bp > 0 AND value_bp <= 10000)),
    value_minor   bigint      CHECK (value_minor IS NULL OR value_minor > 0),
    CONSTRAINT discounts_value_matches_kind CHECK (
        (kind = 'percentage'    AND value_bp IS NOT NULL AND value_minor IS NULL) OR
        (kind = 'fixed'         AND value_minor IS NOT NULL AND value_bp IS NULL) OR
        (kind = 'free_shipping' AND value_bp IS NULL AND value_minor IS NULL)
    ),

    scope              text   NOT NULL DEFAULT 'order'
                              CHECK (scope IN ('order', 'products', 'collections', 'categories')),
    min_subtotal_minor bigint CHECK (min_subtotal_minor IS NULL OR min_subtotal_minor >= 0),

    starts_at     timestamptz,
    ends_at       timestamptz,
    CONSTRAINT discounts_window CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at),

    usage_limit   integer     CHECK (usage_limit IS NULL OR usage_limit > 0),
    used_count    integer     NOT NULL DEFAULT 0 CHECK (used_count >= 0),
    -- Per email, not per customer, and named so. Guest checkout is permanent
    -- (D22), so there is no customer to count against and this is a deterrent
    -- rather than a control. The schema says which.
    once_per_email boolean    NOT NULL DEFAULT false,

    active        boolean     NOT NULL DEFAULT true,
    metadata      jsonb       NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Unique and matched case-insensitively: customers type codes off posters. The
-- folding is in SQL and only in SQL — PostgreSQL's lower() follows the database
-- collation and Go's strings.ToLower does not agree with it on accented
-- characters, and one implementation of case folding is the only version of
-- this that stays true. See the same lesson in taxonomy.go.
CREATE UNIQUE INDEX discounts_code_key ON discounts (lower(code)) WHERE code IS NOT NULL;
CREATE INDEX discounts_active_idx ON discounts (active, id DESC);

CREATE TABLE discount_targets (
    discount_id bigint NOT NULL REFERENCES discounts (id) ON DELETE CASCADE,
    kind        text   NOT NULL CHECK (kind IN ('product', 'collection', 'category')),
    target_id   bigint NOT NULL,
    PRIMARY KEY (discount_id, kind, target_id)
);

CREATE TABLE order_discounts (
    id           bigserial PRIMARY KEY,
    order_id     bigint    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    -- SET NULL rather than RESTRICT: deleting a promotion that ended is a
    -- normal thing to do, and the order keeps its own copy of what it got.
    discount_id  bigint    REFERENCES discounts (id) ON DELETE SET NULL,
    code         text      NOT NULL DEFAULT '',
    title        text      NOT NULL,
    kind         text      NOT NULL,
    amount_minor bigint    NOT NULL CHECK (amount_minor >= 0),
    -- One per order today (D28). The key allows several so that stacking is a
    -- service change later rather than a migration.
    UNIQUE (order_id, title)
);
CREATE INDEX order_discounts_discount_idx ON order_discounts (discount_id);

-- The cart holds what somebody typed. Whether it is real is decided at
-- checkout, under the same lock that re-checks price and stock.
ALTER TABLE carts ADD COLUMN discount_code text NOT NULL DEFAULT '';
`

// M16 — tax.
//
// A rate is a rule about a place and a kind of thing: 18% on electronics sold
// into Karnataka. So a rate carries a country, optionally a state, and
// optionally a category — and the most specific rule that fits a line is the one
// that applies. A rate on "Apparel" reaches "Apparel / Shirts" beneath it,
// because that is what a category tree is for.
//
// What each line was charged is stored on the line. An invoice has to show tax
// per line, and recomputing it later from a rate that has since changed would
// print a different invoice for the same order — which is the one thing an
// invoice may never do.
//
// `orders.tax_inclusive` snapshots which way the store was working when the
// order was placed. Switching a live store from exclusive to inclusive pricing
// changes what every future total means, and every past order has to keep
// meaning what it meant.
const migration0016Taxes = `
CREATE TABLE tax_rates (
    id          bigserial   PRIMARY KEY,
    name        text        NOT NULL CHECK (name <> ''),
    -- Basis points, like a discount: 1800 is 18.00%. Integers all the way down.
    rate_bp     integer     NOT NULL CHECK (rate_bp >= 0 AND rate_bp <= 10000),

    -- Where it applies. An empty country is the fallback every other rule is
    -- more specific than, which is how a single-jurisdiction store configures
    -- one rate and stops thinking about it.
    country     text        NOT NULL DEFAULT '',
    state       text        NOT NULL DEFAULT '',
    -- What it applies to. NULL is everything; a category reaches its whole
    -- subtree. RESTRICT, because deleting a category out from under a live tax
    -- rule should be refused rather than silently widening the rule.
    category_id bigint      REFERENCES categories (id) ON DELETE RESTRICT,

    active      boolean     NOT NULL DEFAULT true,
    metadata    jsonb       NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- One rule per (place, thing). Two rates for the same pair is not a
    -- preference to resolve at checkout, it is a mistake to refuse at entry.
    CONSTRAINT tax_rates_unique_target
        UNIQUE NULLS NOT DISTINCT (country, state, category_id)
);
CREATE INDEX tax_rates_lookup_idx ON tax_rates (active, country, state);

-- What each line was actually charged, snapshotted like its price.
ALTER TABLE order_lines
    ADD COLUMN tax_minor   bigint  NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    ADD COLUMN tax_rate_bp integer NOT NULL DEFAULT 0
               CHECK (tax_rate_bp >= 0 AND tax_rate_bp <= 10000),
    ADD COLUMN tax_name    text    NOT NULL DEFAULT '';

ALTER TABLE orders
    ADD COLUMN tax_minor     bigint  NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    -- Whether the prices on this order already contained the tax. Snapshotted,
    -- because it decides what every figure on the order means.
    ADD COLUMN tax_inclusive boolean NOT NULL DEFAULT false;
`

// M17 — stock lives in a place.
//
// Until now a variant had one number and it was implicitly the whole business:
// `variants.stock_on_hand`. A store with a shop and a warehouse could not say
// which units were where, and every question that matters — can I ship this
// today, which box does this order come out of, what did I actually count last
// Tuesday — needs that answer.
//
// The per-location rows become the only truth and the variant columns go. The
// API keeps `stock_on_hand`, `stock_reserved` and `available` exactly as they
// were, because clients and the MCP module read them; they are now sums across
// locations, computed on the way out. A stored copy of a sum is a number that
// can be wrong, and this codebase has said so before about category paths.
//
// One thing genuinely moves from the database to the service. M12's CHECK —
// reserved may not pass on-hand unless the variant sells past zero — cannot be
// written on a row that does not carry `continue_selling`, and copying the flag
// onto every stock row would be the same stored-duplicate mistake. What replaces
// it is the guard that was always doing the real work: reserveStock's
// conditional UPDATE, which refuses in the same statement that decrements, so
// there is no window between the check and the change. `doctor` verifies the
// invariant across every row, which is where a drift would surface.
const migration0017Locations = `
CREATE TABLE locations (
    id         bigserial   PRIMARY KEY,
    code       text        NOT NULL UNIQUE CHECK (code <> ''),
    name       text        NOT NULL CHECK (name <> ''),
    address    jsonb       NOT NULL DEFAULT '{}',
    -- Lower is preferred. Where a reservation may come from more than one
    -- place, it is taken from the first that can cover it.
    priority   integer     NOT NULL DEFAULT 0,
    active     boolean     NOT NULL DEFAULT true,
    is_default boolean     NOT NULL DEFAULT false,
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Exactly one default, enforced rather than remembered: "which location is this
-- going out of" must always have an answer, and two answers is worse than none.
CREATE UNIQUE INDEX locations_one_default ON locations ((is_default)) WHERE is_default;

CREATE TABLE variant_stock (
    variant_id  bigint      NOT NULL REFERENCES variants (id) ON DELETE CASCADE,
    -- RESTRICT: closing a location that still holds stock is a decision about
    -- the stock, and it has to be made before the location can go.
    location_id bigint      NOT NULL REFERENCES locations (id) ON DELETE RESTRICT,
    on_hand     integer     NOT NULL DEFAULT 0,
    reserved    integer     NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (variant_id, location_id)
);
CREATE INDEX variant_stock_location_idx ON variant_stock (location_id);

-- Everything that exists today is in one place, and this is that place. The
-- name is deliberately plain: a store with one location should never have to
-- think about locations at all.
INSERT INTO locations (code, name, is_default, priority)
VALUES ('default', 'Main location', true, 0);

INSERT INTO variant_stock (variant_id, location_id, on_hand, reserved)
SELECT v.id, l.id, v.stock_on_hand, v.stock_reserved
FROM variants v CROSS JOIN locations l
WHERE l.is_default;

-- Where a line's units came from, so cancelling puts them back where they were
-- rather than wherever the default happens to be today. SET NULL because an
-- order is a snapshot and must stay readable after a location is closed.
ALTER TABLE order_lines
    ADD COLUMN location_id bigint REFERENCES locations (id) ON DELETE SET NULL;
UPDATE order_lines SET location_id = (SELECT id FROM locations WHERE is_default);

-- The columns go last, so the backfill above reads them.
ALTER TABLE variants
    DROP COLUMN stock_on_hand,
    DROP COLUMN stock_reserved;
`

// M18 — inviting somebody instead of inventing their password.
//
// Until now the only way onto a team was for an owner to type a password on
// somebody else's behalf and then tell it to them: over chat, or out loud. That
// password is known to two people from the moment it exists, it usually never
// gets changed, and the store has no way to tell whether the person on the other
// end ever received it. An invitation replaces all of that — the invitee sets a
// password nobody else has seen, and the store can see who has and has not
// joined.
//
// Only the hash of the token is stored, exactly as with superuser_sessions: a
// leaked database yields no usable invitation, and the token exists solely in
// the link the invitee was sent.
const migration0018Invitations = `
CREATE TABLE superuser_invitations (
    id          bigserial   PRIMARY KEY,
    -- Normalised the same way superusers.email is, so the open-invitation index
    -- below actually catches a second invite to the same person.
    email       text        NOT NULL CHECK (email <> ''),
    role        text        NOT NULL CHECK (role <> ''),
    token_hash  text        NOT NULL UNIQUE,
    -- SET NULL rather than CASCADE: who invited whom is a fact about the past,
    -- and it should survive that person leaving. The invitation is still valid.
    invited_by  bigint      REFERENCES superusers (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    -- NULL while outstanding. Kept rather than deleted on acceptance, so the
    -- team screen can answer "who let this person in, and when".
    accepted_at timestamptz
);

-- One outstanding invitation per address. A second one would leave two live
-- links for the same person, and revoking the one the owner can see would not
-- close the door.
CREATE UNIQUE INDEX superuser_invitations_open
    ON superuser_invitations (email) WHERE accepted_at IS NULL;
CREATE INDEX superuser_invitations_expiry_idx ON superuser_invitations (expires_at);
`
