CREATE TABLE runtime_identity (
 id INTEGER PRIMARY KEY CHECK (id=1),
 telegram_family TEXT NOT NULL CHECK (telegram_family IN ('mock','live'))
);
