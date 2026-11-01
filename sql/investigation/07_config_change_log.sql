-- What MAPPING_FIXES.sql changed, from the audit trigger.
SELECT id, config, op, config_id,
       old_row ->> 'to_summary_field_when_positive_amount' AS old_pos,
       new_row ->> 'to_summary_field_when_positive_amount' AS new_pos,
       old_row ->> 'description_match' AS old_match, new_row ->> 'description_match' AS new_match
FROM config_change_log ORDER BY id;
