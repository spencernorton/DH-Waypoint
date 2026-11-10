package csamblocklist

// removalSeed inserts one row per table that can hold a torrent's name or a
// file path. Every statement takes $1 = info hash (bytea, 20 bytes),
// $2 = torrent name (text), $3 = a file path (text). Run in order.
// Keys derived from $1 use sha256 so two hashes never collide.
var removalSeed = []struct{ table, sql string }{
	{"torrents", `insert into torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)
 select $1::bytea,$2::text,4096,false,'multi',1,now(),now() where $3::text is not null`},
	{"torrent_files", `insert into torrent_files(info_hash,"index",path,size,created_at,updated_at)
 select $1::bytea,0,$3::text,4096,now(),now() where $2::text is not null`},
	{"torrent_sources", `insert into torrent_sources(key,name,created_at,updated_at)
 select 'seed-'||encode(sha256($1::bytea),'hex'),'Seed source',now(),now() where $2::text is not null and $3::text is not null`},
	{"torrents_torrent_sources", `insert into torrents_torrent_sources(source,info_hash,import_id,seeders,leechers,published_at,created_at,updated_at)
 select 'seed-'||encode(sha256($1::bytea),'hex'),$1::bytea,'seed-import',1,1,now(),now(),now() where $2::text is not null and $3::text is not null`},
	{"torrent_hints", `insert into torrent_hints(info_hash,content_type,content_id,title,created_at,updated_at)
 select $1::bytea,'movie',null,$2::text,now(),now() where $3::text is not null`},
	{"torrent_contents", `insert into torrent_contents(info_hash,created_at,updated_at,tsv,size,files_count)
 select $1::bytea,now(),now(),to_tsvector('simple',$2::text),4096,1 where $3::text is not null`},
	{"torrent_tags", `insert into torrent_tags(info_hash,name,created_at,updated_at)
 select $1::bytea,'ordinary',now(),now() where $2::text is not null and $3::text is not null`},
	{"torrent_pieces", `insert into torrent_pieces(info_hash,piece_length,pieces,created_at)
 select $1::bytea,16384,decode('00ff1234','hex'),now() where $2::text is not null and $3::text is not null`},
	{"torrent_tracker_seeds", `insert into torrent_tracker_seeds(info_hash,tracker_known,seeders,checked_at)
 select $1::bytea,true,1,now() where $2::text is not null and $3::text is not null`},
	{"junkpurge_sync_claims", `insert into junkpurge_sync_claims(info_hash,owner,torrent_name,claimed_at,lease_until)
 select $1::bytea,'seed-owner',$2::text,now()-interval '1 hour',now() where $3::text is not null`},
	{"junkpurge_quarantine", `insert into junkpurge_quarantine(info_hash,torrent_name,verdict,confidence,torrent_snapshot,files_snapshot,sources_snapshot)
 select $1::bytea,$2::text,'junk',0.9,
  jsonb_build_object('name',$2::text,'path',$3::text),
  jsonb_build_array(jsonb_build_object('name',$2::text,'path',$3::text)),
  jsonb_build_array(jsonb_build_object('name',$2::text,'path',$3::text))`},
	{"junkpurge_judgments", `insert into junkpurge_judgments(info_hash,verdict,confidence,reason,torrent_name)
 select $1::bytea,'junk',0.9,$3::text,$2::text`},
	{"junkpurge_batch_runs", `insert into junkpurge_batch_runs(state,model,provider_base_url,endpoint,prompt_version,system_prompt,max_completion_tokens,reasoning_effort,completion_window,min_age_seconds,min_confidence,max_junk_rate,enable_purge,quarantine_days,failure_cooldown_seconds,max_attempts,ambiguity_grace_seconds,item_count)
 select 'completed','seed-'||encode(sha256($1::bytea),'hex'),'http://seed.invalid','/v1/chat','seed','seed prompt',16,'low','24h',60,0.5,0.5,false,7,60,3,60,1 where $2::text is not null and $3::text is not null`},
	{"junkpurge_batch_attempts", `insert into junkpurge_batch_attempts(run_id,attempt_no,state,input_filename,input_sha256,input_payload,input_bytes,item_count)
 select r.id,1,'completed','seed.jsonl',encode(sha256($1::bytea),'hex'),convert_to($2::text||' '||$3::text,'UTF8'),10,1
 from junkpurge_batch_runs r where r.model='seed-'||encode(sha256($1::bytea),'hex')`},
	{"junkpurge_batch_items", `insert into junkpurge_batch_items(run_id,ordinal,custom_id,info_hash,torrent_name,state,last_attempt_id,error_code,error_message)
 select r.id,1,'seed-'||encode(sha256($1::bytea),'hex'),$1::bytea,$2::text,'terminal_error',
  (select a.id from junkpurge_batch_attempts a where a.run_id=r.id and a.attempt_no=1),'seed',$2::text||' '||$3::text
 from junkpurge_batch_runs r where r.model='seed-'||encode(sha256($1::bytea),'hex')`},
	{"llm_work_tasks", `insert into llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,reason,expires_at)
 select sha256($1::bytea||'\x01'::bytea),'matcher',$1::bytea,sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),
  jsonb_build_object('name',$2::text,'path',$3::text),1,0,1,1,'completed','seed',now()+interval '1 day'`},
	{"llm_work_events", `insert into llm_work_events(task_key,state,reason)
 select task_key,'completed','seed' from llm_work_tasks where info_hash=$1::bytea and $2::text is not null and $3::text is not null`},
	{"llm_work_applications", `insert into llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)
 select task_key,info_hash,source_digest,policy_digest,jsonb_build_object('name',$2::text,'path',$3::text)
 from llm_work_tasks where info_hash=$1::bytea`},
	{"processor_legacy_freeze", `insert into processor_legacy_freeze(task_key,snapshot) select task_key,to_jsonb(t) from llm_work_tasks t where info_hash=$1::bytea and $2::text is not null and $3::text is not null`},
	{"llm_evaluation_captures", `insert into llm_evaluation_captures(capture_key,task,source_sha256,group_sha256,input_sha256,contract_sha256,prompt_sha256,endpoint_sha256,model,prompt_version,build_identity,contract_id,system_prompt,model_input,task_input,capture_schema_version,privacy_status,sampling_origin,captured_at,expires_at,privacy_checked_at)
 select sha256($1::bytea||'\x02'::bytea),'junkpurge',sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),sha256($1::bytea),
  'seed-model','seed','seed','seed','seed prompt',
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text),
  1,'verified_native_public_qb_rechecked','natural_capture',now(),now()+interval '1 day',now()`},
	{"llm_evaluation_capture_admissions", `insert into llm_evaluation_capture_admissions(capture_key,info_hash,expires_at)
 select sha256($1::bytea||'\x02'::bytea),$1::bytea,now()+interval '1 day' where $2::text is not null and $3::text is not null`},
	{"llm_evaluation_capture_results", `insert into llm_evaluation_capture_results(capture_key,response_body,response_sha256,http_status,error_class,observed_at,decision,decided_at)
 select sha256($1::bytea||'\x02'::bytea),convert_to($2::text||' '||$3::text,'UTF8'),sha256($1::bytea),200,'none',now(),
  jsonb_build_object('name',$2::text,'path',$3::text),now()`},
	{"processor_jobs", `insert into processor_jobs(job_id,info_hash,input_revision,pipeline_revision,epoch,input,source,plan,prepared_job,results)
 select encode(sha256($1::bytea),'hex'),$1::bytea,encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),1,
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text),'{}'::jsonb,
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_array(jsonb_build_object('name',$2::text,'path',$3::text))`},
	{"processor_attempts", `insert into processor_attempts(job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,reason)
 select encode(sha256($1::bytea),'hex'),1,'seed-processor',1,'seed',0,$2::text||' '||$3::text`},
	{"processor_applications", `insert into processor_applications(info_hash,job_id,input_revision,applied_revision,pipeline_revision,results)
 select $1::bytea,encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),
  jsonb_build_array(jsonb_build_object('name',$2::text,'path',$3::text))`},
	{"processor_exclusions", `insert into processor_exclusions(info_hash,job_id,processor_id,binding,reason)
 select $1::bytea,encode(sha256($1::bytea),'hex'),'seed-processor',jsonb_build_object('name',$2::text,'path',$3::text),$2::text||' '||$3::text`},
	{"processor_feedback_events", `insert into processor_feedback_events(event_id,processor_id,execution_generation,info_hash,tier,input,prediction,trace,ground_truth)
 select encode(sha256($1::bytea),'hex'),'seed-processor',1,$1::bytea,'operational_receipt',
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text),
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text)`},
	{"processor_reprocess_requests", `insert into processor_reprocess_requests(request_id,request_digest,origin_job_id,info_hash,expected_pipeline_revision,reason,state,core_job_id,origin_export,before_snapshot)
 select encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),encode(sha256($1::bytea),'hex'),$1::bytea,encode(sha256($1::bytea),'hex'),'seed','settled',encode(sha256($1::bytea),'hex'),
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text)`},
	{"release_field_repair_journal", `insert into release_field_repair_journal(plan_digest,target_id,info_hash,source_name_sha256,before_fields,after_fields,application_before,application_after,applied_updated_at)
 select sha256($1::bytea),'seed-target',$1::bytea,encode(sha256($1::bytea),'hex'),
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text),
  jsonb_build_object('name',$2::text,'path',$3::text),jsonb_build_object('name',$2::text,'path',$3::text),now()`},
	{"catalogue_policy_shadow", `insert into catalogue_policy_shadow(info_hash,receipt)
 select $1::bytea,jsonb_build_object('name',$2::text,'path',$3::text)`},
	{"eval_frozen", `insert into eval_frozen(segment,info_hash,name,expected)
 select 'seed',$1::bytea,$2::text,jsonb_build_object('name',$2::text,'path',$3::text)`},
	{"private_torrent_files", `insert into private_torrent_files(info_hash,torrent_file)
 select $1::bytea,convert_to($2::text||' '||$3::text,'UTF8')`},
	{"torrent_verdict_events", `insert into torrent_verdict_events(info_hash,verdict,mechanism,reason,evidence)
 select $1::bytea,'active','operator',$2::text,jsonb_build_object('name',$2::text,'path',$3::text)`},
	{"torrent_verdict_state", `insert into torrent_verdict_state(info_hash,verdict,mechanism,since)
 select $1::bytea,'active','operator',now() where $2::text is not null and $3::text is not null`},
	{"catalogue_recovery_snapshots", `insert into catalogue_recovery_snapshots(info_hash,source_digest,contract_version,state,reason,snapshot,payload_bytes,created_at,expires_at,removed_verdict_event_id)
 select $1::bytea,encode(sha256($1::bytea),'hex'),'seed','removed',$2::text,jsonb_build_object('name',$2::text,'path',$3::text),10,now(),now()+interval '1 day',
  (select max(id) from torrent_verdict_events where info_hash=$1::bytea)`},
	{"catalogue_recovery_events", `insert into catalogue_recovery_events(snapshot_id,transition,observed_at,reason)
 select id,'removed',now(),$2::text from catalogue_recovery_snapshots where info_hash=$1::bytea and $3::text is not null`},
	{"processor_core_receipts", `insert into processor_core_receipts(info_hash,source_digest,content_id,core_policy,processing_outcome,outcome)
 select $1::bytea,sha256($1::bytea),'seed','seed','completed','matched' where $2::text is not null and $3::text is not null`},
	{"torrent_liveness", `insert into torrent_liveness(info_hash,status,last_observed_at)
 select $1::bytea,'alive',now() where $2::text is not null and $3::text is not null`},
	{"junkpurge_student_releases", `insert into junkpurge_student_releases(info_hash,p_junk,threshold,model_id)
 select $1::bytea,0.5,0.5,'seed-model' where $2::text is not null and $3::text is not null`},
}
