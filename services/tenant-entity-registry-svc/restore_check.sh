#!/bin/sh
# Compare row counts
echo "1. Row count comparison:"
for t in $(psql -U postgres -d postgres -tAc "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1"); do
  a=$(psql -U postgres -d postgres -tAc "SELECT count(*) FROM \"$t\"")
  b=$(psql -U postgres -d tenant_entity_registry_restore -tAc "SELECT count(*) FROM \"$t\"")
  if [ "$a" = "$b" ]; then
    echo "  PASS $t: $a rows"
  else
    echo "  FAIL $t: $a -> $b"
  fi
done

# Check FORCE RLS
echo ""
echo "2. FORCE RLS comparison:"
q_force="SELECT string_agg(relname, ',' ORDER BY relname) FROM pg_class WHERE relkind='r' AND relforcerowsecurity"
force1=$(psql -U postgres -d postgres -tAc "$q_force")
force2=$(psql -U postgres -d tenant_entity_registry_restore -tAc "$q_force")
if [ "$force1" = "$force2" ]; then
  echo "  PASS FORCE RLS tables match"
else
  echo "  FAIL FORCE RLS differs"
  echo "    Original: $force1"
  echo "    Restored: $force2"
fi

# Check policies
echo ""
echo "3. Policy comparison:"
q_pol="SELECT string_agg(tablename||':'||policyname||':'||coalesce(qual,'')||':'||coalesce(with_check,''), '|' ORDER BY tablename, policyname) FROM pg_policies WHERE schemaname='public'"
pol1=$(psql -U postgres -d postgres -tAc "$q_pol")
pol2=$(psql -U postgres -d tenant_entity_registry_restore -tAc "$q_pol")
if [ "$pol1" = "$pol2" ]; then
  echo "  PASS Policies match"
else
  echo "  FAIL Policies differ"
fi

# Check CHECK constraints
echo ""
echo "4. CHECK constraints comparison:"
q_con="SELECT string_agg(conname, ',' ORDER BY conname) FROM pg_constraint WHERE contype='c'"
con1=$(psql -U postgres -d postgres -tAc "$q_con")
con2=$(psql -U postgres -d tenant_entity_registry_restore -tAc "$q_con")
if [ "$con1" = "$con2" ]; then
  echo "  PASS CHECK constraints match"
else
  echo "  FAIL CHECK constraints differ"
  echo "    Original: $con1"
  echo "    Restored: $con2"
fi

# Check history invariants
echo ""
echo "5. History invariants:"
overlap=$(psql -U postgres -d tenant_entity_registry_restore -tAc "SELECT count(*) FROM (SELECT legal_entity_id FROM legal_entity_profile_versions WHERE effective_to IS NULL GROUP BY 1 HAVING count(DISTINCT effective_from) > 1) x")
if [ "$overlap" = "0" ]; then
  echo "  PASS No entity has two open profile versions"
else
  echo "  FAIL Found $overlap entities with overlapping open versions"
fi

ver=$(psql -U postgres -d tenant_entity_registry_restore -tAc "SELECT count(*) FROM tenants t WHERE t.record_version < 1")
if [ "$ver" = "0" ]; then
  echo "  PASS Tenant versions intact"
else
  echo "  FAIL Found $ver tenants with record_version < 1"
fi

unpub=$(psql -U postgres -d tenant_entity_registry_restore -tAc "SELECT count(*) FROM event_outbox WHERE published_at IS NULL")
echo "  INFO $unpub outbox events unpublished in the copy (at-least-once delivery, consumers dedupe on event_id)"