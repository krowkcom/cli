//! krowk.db: schema, migrations and queries for the local session store.
//!
//! Port of internal/store. The schema is the compatibility boundary with the
//! Go build — a krowk.db either binary wrote must open in the other until the
//! Go build is gone.
