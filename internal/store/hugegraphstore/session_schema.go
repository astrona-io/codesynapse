package hugegraphstore

const sessionSchema = `def s=graph.schema()
['session_id'].each{s.propertyKey(it).asText().ifNotExist().create()}
['session_revision','expires','facts'].each{s.propertyKey(it).asLong().ifNotExist().create()}
s.propertyKey('deleted').asBoolean().ifNotExist().create()
s.vertexLabel('SessionMemoryScope').useCustomizeStringId().properties('tenant_id','session_id','ns','doc','session_revision','expires','deleted','facts').ifNotExist().create()
s.vertexLabel('SessionMemoryRevocation').useCustomizeStringId().properties('tenant_id','session_id').ifNotExist().create()
s.vertexLabel('SessionMemoryEntity').useCustomizeStringId().properties('eid','tenant_id','session_id','ns','name','doc','expires').ifNotExist().create()
s.edgeLabel('SESSION_FACT').sourceLabel('SessionMemoryEntity').targetLabel('SessionMemoryEntity').multiTimes().properties('fid','tenant_id','session_id','ns','predicate','doc','expires').sortKeys('fid').ifNotExist().create()
s.indexLabel('sessionScopeIdentity').onV('SessionMemoryScope').by('tenant_id','session_id','ns').secondary().ifNotExist().create()
s.indexLabel('sessionRevocation').onV('SessionMemoryRevocation').by('tenant_id','session_id').secondary().ifNotExist().create()
s.indexLabel('sessionEntityScope').onV('SessionMemoryEntity').by('tenant_id','session_id','ns').secondary().ifNotExist().create()
s.indexLabel('sessionFactScope').onE('SESSION_FACT').by('tenant_id','session_id','ns').secondary().ifNotExist().create()
1`
