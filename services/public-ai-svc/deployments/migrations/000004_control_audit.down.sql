DROP TRIGGER IF EXISTS control_audit_append_only ON public_ai.control_audit;
DROP FUNCTION IF EXISTS public_ai.reject_control_audit_change();
DROP TABLE IF EXISTS public_ai.control_audit;
