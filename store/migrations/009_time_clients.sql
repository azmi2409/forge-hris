-- 009: allow slack/teams as time-entry client types (chat-bot punches).
ALTER TABLE time_entries DROP CONSTRAINT IF EXISTS time_entries_client_type_check;
ALTER TABLE time_entries ADD CONSTRAINT time_entries_client_type_check
    CHECK (client_type IN ('web', 'mobile', 'desktop', 'kiosk', 'ai_assistant', 'slack', 'teams'));
